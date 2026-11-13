package main

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var ifaceNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,15}$`)

func nftList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + n + `"`
	}
	return strings.Join(q, ", ")
}

// renderNft is deploy-nft.md 3.1: every IPv4 TCP and UDP connection from the LAN that is not for this
// machine goes to sing-box. The main router only sends this machine what is to be proxied (the FakeIP
// range and the networks to proxy), so sing-box decides all of it: a FakeIP by its binding or Final, a
// real address by the IP, port and network rules, else Final. reply (single-NIC only, 3.4) marks these
// connections and sends their answers through the main router, so that it sees both directions.
func renderNft(lan []string, reply bool) (string, error) {
	if err := checkIfaceNames(lan); err != nil {
		return "", err
	}
	l := nftList(lan)
	ct, out := "", ""
	if reply {
		ct = "ct mark set " + replyMark + " "
		out = fmt.Sprintf(`

	# Answers to the connections above go back through the main router (policy routing, table %[2]s).
	chain output_reply {
		type route hook output priority mangle; policy accept;
		ct direction reply ct mark %[1]s meta mark set %[1]s
	}`, replyMark, replyTable)
	}
	return fmt.Sprintf(`#!/usr/sbin/nft -f
# atom: send TCP and UDP from the LAN to sing-box. Written by atom-installer.
table inet atom
delete table inet atom
table inet atom {
	# TCP: redirect to sing-box redirect-in (7892).
	chain prerouting_tcp {
		type nat hook prerouting priority dstnat; policy accept;
		iifname != { %[1]s } return
		fib daddr type { local, broadcast, multicast } return
		meta nfproto ipv4 meta l4proto tcp %[3]sredirect to :7892
	}

	# UDP: TProxy to sing-box tproxy-in (7893); the mark sends it to table 233 (local). The
	# replies of a flow opened from elsewhere (the WAN) are forwarded as they are.
	chain prerouting_udp {
		type filter hook prerouting priority mangle; policy accept;
		iifname != { %[1]s } return
		fib daddr type { local, broadcast, multicast } return
		ct direction reply return
		meta nfproto ipv4 meta l4proto udp %[3]stproxy ip to :7893 meta mark set %[2]s accept
	}%[4]s
}
`, l, fwmark, ct, out), nil
}

// renderGatewayNft is deploy-nft.md 3.2: what is neither TCP nor UDP (ping) is forwarded and NATed out of
// the WAN. The panel is
// for the LAN only: its port is closed on the WAN (atomdns still listens on every address, so the installer
// reaches it on 127.0.0.1 and a changed LAN address does not matter).
func renderGatewayNft(lan []string, wan string, panelPort int) (string, error) {
	if err := checkIfaceNames(append(append([]string{}, lan...), wan)); err != nil {
		return "", err
	}
	if panelPort < 1 || panelPort > 65535 {
		return "", fmt.Errorf("invalid panel port %d", panelPort)
	}
	return fmt.Sprintf(`#!/usr/sbin/nft -f
# atom, dual-NIC gateway only: what is neither TCP nor UDP from the LAN is forwarded and NATed out of the WAN. Written by atom-installer.
table ip atom_nat
delete table ip atom_nat
table ip atom_nat {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		iifname { %[1]s } oifname "%[2]s" masquerade
	}

	# The panel is not reachable from the WAN.
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%[2]s" tcp dport %[3]d drop
	}
}
`, nftList(lan), wan, panelPort), nil
}

func checkIfaceNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("no interface given")
	}
	for _, n := range names {
		if !ifaceNameRe.MatchString(n) {
			return fmt.Errorf("invalid interface name %q", n)
		}
	}
	return nil
}

// renderPolicyRoute is deploy-nft.md 3.3; ip rule is added only when it is not there yet. The client routes
// of a dual-NIC gateway follow: answers to clients behind the LAN-side router go back through the LAN. reply
// (single-NIC, 3.4) is the route of the marked answers through the main router.
func renderPolicyRoute(routes []ClientRoute, reply *ClientRoute) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, `#!/bin/sh
# atom: policy routing for the TProxy mark. Written by atom-installer.
set -e
ip rule show | grep -q 'fwmark %[1]s lookup %[2]s' || ip rule add fwmark %[1]s lookup %[2]s priority %[3]s
ip route replace local 0.0.0.0/0 dev lo table %[2]s
`, fwmark, routeTable, routePriority)
	for _, r := range routes {
		if err := r.check(); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "ip route replace %s via %s dev %s\n", r.Net, r.Via, r.Dev)
	}
	if reply != nil {
		if err := reply.check(); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "ip rule show | grep -q 'from all fwmark %[1]s lookup %[2]s' || ip rule add fwmark %[1]s lookup %[2]s priority %[3]s\n", replyMark, replyTable, replyPriority)
		fmt.Fprintf(&b, "ip route replace default via %s dev %s table %s\n", reply.Via, reply.Dev, replyTable)
	}
	return b.String(), nil
}

// check makes sure a recorded route is safe to put into a shell script.
func (r ClientRoute) check() error {
	ip, n, err := net.ParseCIDR(r.Net)
	if err != nil || ip.To4() == nil || n.String() != r.Net {
		return fmt.Errorf("invalid client network %q", r.Net)
	}
	if v := net.ParseIP(r.Via); v == nil || v.To4() == nil {
		return fmt.Errorf("invalid LAN gateway %q", r.Via)
	}
	return checkIfaceNames([]string{r.Dev})
}

// renderDNSUnit is deploy-nft.md 4: load the rules and the routing before atomdns starts. replyDev (3.4) is
// the LAN interface that must not take ICMP redirects from the main router.
func renderDNSUnit(p Paths, nftBin, sysctlBin string, gateway bool, replyDev string) string {
	var pre strings.Builder
	fmt.Fprintf(&pre, "ExecStartPre=%s -f %s\n", nftBin, p.Etc("atom.nft"))
	if gateway {
		fmt.Fprintf(&pre, "ExecStartPre=%s -f %s\n", nftBin, p.Etc("atom-gateway.nft"))
		fmt.Fprintf(&pre, "ExecStartPre=%s -w net.ipv4.ip_forward=1\n", sysctlBin)
	}
	if replyDev != "" {
		fmt.Fprintf(&pre, "ExecStartPre=%s -w net/ipv4/conf/all/accept_redirects=0 net/ipv4/conf/%s/accept_redirects=0\n", sysctlBin, replyDev)
	}
	fmt.Fprintf(&pre, "ExecStartPre=%s\n", p.Lib("policy-route.sh"))
	return fmt.Sprintf(`[Unit]
Description=atom DNS (AtomDNS fork: rules, FakeIP and the control plane)
After=network-online.target
Wants=network-online.target

[Service]
%[1]sExecStart=%[2]s start -d %[3]s -c config_custom.yaml
Restart=on-failure
RestartSec=2
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`, pre.String(), p.Bin("atomdns"), p.Data())
}

// renderSingBoxUnit is design 6: started and reloaded by atomdns (systemctl reload-or-restart).
func renderSingBoxUnit(p Paths) string {
	dir := p.Data() + "/sing-box"
	return fmt.Sprintf(`[Unit]
Description=atom sing-box (FakeIP outbound fork), configured by atom-dns
After=network-online.target
ConditionPathExists=%[2]s/config.json

[Service]
ExecStart=%[1]s run -D %[2]s -c %[2]s/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=1
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`, p.Bin("sing-box"), dir)
}

var panelPortRe = regexp.MustCompile(`(?m)^(api:[ \t]*\r?\n[ \t]+http:[ \t]*")[^"]*(")`)

// setPanelPort rewrites api.http of config_custom.yaml to 0.0.0.0:<port>.
func setPanelPort(yaml string, port int) (string, error) {
	if !panelPortRe.MatchString(yaml) {
		return "", fmt.Errorf("config_custom.yaml has no api.http line")
	}
	return panelPortRe.ReplaceAllString(yaml, "${1}0.0.0.0:"+strconv.Itoa(port)+"${2}"), nil
}

// renderDNSInit is renderDNSUnit for OpenRC: supervise-daemon restarts atomdns when it exits (Restart=on-failure),
// and what the unit does in ExecStartPre happens in start_pre.
func renderDNSInit(p Paths, nftBin, sysctlBin string, gateway bool, replyDev string) string {
	var pre strings.Builder
	fmt.Fprintf(&pre, "\t%s -f %s || return 1\n", nftBin, p.Etc("atom.nft"))
	if gateway {
		fmt.Fprintf(&pre, "\t%s -f %s || return 1\n", nftBin, p.Etc("atom-gateway.nft"))
		fmt.Fprintf(&pre, "\t%s -w net.ipv4.ip_forward=1 || return 1\n", sysctlBin)
	}
	if replyDev != "" {
		fmt.Fprintf(&pre, "\t%s -w net/ipv4/conf/all/accept_redirects=0 net/ipv4/conf/%s/accept_redirects=0 || return 1\n", sysctlBin, replyDev)
	}
	fmt.Fprintf(&pre, "\t%s || return 1\n", p.Lib("policy-route.sh"))
	return fmt.Sprintf(`#!/sbin/openrc-run
# atom DNS (AtomDNS fork: rules, FakeIP and the control plane). Written by atom-installer.
description="atom DNS"
supervisor=supervise-daemon
command=%[2]s
command_args="start -d %[3]s -c config_custom.yaml"
respawn_delay=2
respawn_max=0
rc_ulimit="-n 1048576"
output_log=%[4]s/atom-dns.log
error_log=%[4]s/atom-dns.log

depend() {
	need net
}

start_pre() {
	checkpath -d -m 0755 %[4]s
%[1]s}
`, pre.String(), p.Bin("atomdns"), p.Data(), openRCLogDir)
}

// renderSingBoxInit is renderSingBoxUnit for OpenRC: reload sends SIGHUP; without config.json it does not start.
func renderSingBoxInit(p Paths) string {
	dir := p.Data() + "/sing-box"
	return fmt.Sprintf(`#!/sbin/openrc-run
# atom sing-box (FakeIP outbound fork), configured by atom-dns. Written by atom-installer.
description="atom sing-box"
supervisor=supervise-daemon
command=%[1]s
command_args="run -D %[2]s -c %[2]s/config.json"
respawn_delay=1
respawn_max=0
rc_ulimit="-n 1048576"
output_log=%[3]s/atom-sing-box.log
error_log=%[3]s/atom-sing-box.log
extra_started_commands="reload"

depend() {
	need net
}

start_pre() {
	[ -f %[2]s/config.json ] || { eerror "no %[2]s/config.json"; return 1; }
	checkpath -d -m 0755 %[3]s
}

reload() {
	ebegin "Reloading $RC_SVCNAME"
	supervise-daemon "$RC_SVCNAME" --signal HUP
	eend $?
}
`, p.Bin("sing-box"), dir, openRCLogDir)
}

// renderReloadScript is the reload command of atom_rules.yaml on OpenRC: reload sing-box when it runs, else
// start it (what systemctl reload-or-restart does).
func renderReloadScript() string {
	return `#!/bin/sh
# atom: reload sing-box when it runs, else start it. Written by atom-installer.
if rc-service atom-sing-box status >/dev/null 2>&1; then
	exec rc-service atom-sing-box reload
fi
exec rc-service atom-sing-box start
`
}

var reloadCmdRe = regexp.MustCompile(`(?m)^([ \t]+reload:[ \t]*)\[[^\]\n]*\]`)

// setReloadCommand rewrites the reload command of atom_rules.yaml (singbox.reload) to run cmd.
func setReloadCommand(yaml, cmd string) (string, error) {
	if !reloadCmdRe.MatchString(yaml) {
		return "", fmt.Errorf("atom_rules.yaml has no reload command")
	}
	return reloadCmdRe.ReplaceAllString(yaml, "${1}["+strings.ReplaceAll(cmd, "$", "$$")+"]"), nil
}
