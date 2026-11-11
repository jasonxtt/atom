package main

import (
	"fmt"
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

// renderGatewayNft is deploy-nft.md 3.2: everything else from the LAN is NATed out of the WAN.
func renderGatewayNft(lan []string, wan string) (string, error) {
	if err := checkIfaceNames(append(append([]string{}, lan...), wan)); err != nil {
		return "", err
	}
	return fmt.Sprintf(`#!/usr/sbin/nft -f
# atom, dual-NIC gateway only: everything else from the LAN is forwarded and NATed out of the WAN. Written by atom-installer.
table ip atom_nat
delete table ip atom_nat
table ip atom_nat {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		iifname { %s } oifname "%s" masquerade
	}
}
`, nftList(lan), wan), nil
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

// renderPolicyRoute is deploy-nft.md 3.3; ip rule is added only when it is not there yet.
func renderPolicyRoute() string {
	return fmt.Sprintf(`#!/bin/sh
# atom: policy routing for the TProxy mark. Written by atom-installer.
set -e
ip rule show | grep -q 'fwmark %[1]s lookup %[2]s' || ip rule add fwmark %[1]s lookup %[2]s priority %[3]s
ip route replace local 0.0.0.0/0 dev lo table %[2]s
`, fwmark, routeTable, routePriority)
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
