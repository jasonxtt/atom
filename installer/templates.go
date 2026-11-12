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

// renderNft is deploy-nft.md 3.1: the FakeIP and IP-rule destinations from the LAN go to sing-box.
func renderNft(lan []string) (string, error) {
	if err := checkIfaceNames(lan); err != nil {
		return "", err
	}
	l := nftList(lan)
	return fmt.Sprintf(`#!/usr/sbin/nft -f
# atom: send FakeIP and IP-rule destinations from the LAN to sing-box. Written by atom-installer.
table inet atom
delete table inet atom
table inet atom {
	# Filled by atomdns (nft -f, one transaction) at start and after each application.
	set fakeip4 {
		type ipv4_addr
		flags interval
		auto-merge
	}
	set iprule4 {
		type ipv4_addr
		flags interval
		auto-merge
	}

	# TCP: redirect to sing-box redirect-in (7892).
	chain prerouting_tcp {
		type nat hook prerouting priority dstnat; policy accept;
		iifname != { %[1]s } return
		fib daddr type local return
		ip daddr @fakeip4 meta l4proto tcp redirect to :7892
		ip daddr @iprule4 meta l4proto tcp redirect to :7892
	}

	# UDP: TProxy to sing-box tproxy-in (7893); the mark sends it to table 233 (local).
	chain prerouting_udp {
		type filter hook prerouting priority mangle; policy accept;
		iifname != { %[1]s } return
		fib daddr type local return
		ip daddr @fakeip4 meta l4proto udp tproxy ip to :7893 meta mark set %[2]s accept
		ip daddr @iprule4 meta l4proto udp tproxy ip to :7893 meta mark set %[2]s accept
	}
}
`, l, fwmark), nil
}

// renderGatewayNft is deploy-nft.md 3.2: everything else from the LAN is NATed out of the WAN. The panel is
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
# atom, dual-NIC gateway only: everything else from the LAN is forwarded and NATed out of the WAN. Written by atom-installer.
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
// of a dual-NIC gateway follow: answers to clients behind the LAN-side router go back through the LAN.
func renderPolicyRoute(routes []ClientRoute) (string, error) {
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

// renderDNSUnit is deploy-nft.md 4: load the rules and the routing first, atomdns then fills the sets.
func renderDNSUnit(p Paths, nftBin, sysctlBin string, gateway bool) string {
	var pre strings.Builder
	fmt.Fprintf(&pre, "ExecStartPre=%s -f %s\n", nftBin, p.Etc("atom.nft"))
	if gateway {
		fmt.Fprintf(&pre, "ExecStartPre=%s -f %s\n", nftBin, p.Etc("atom-gateway.nft"))
		fmt.Fprintf(&pre, "ExecStartPre=%s -w net.ipv4.ip_forward=1\n", sysctlBin)
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
