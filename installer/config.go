package main

import (
	"fmt"
	"net"
	"strings"
)

// Config is what the wizard collects.
type Config struct {
	Topology      string   `json:"topology"` // bypass | gateway
	LAN           []string `json:"lan"`
	WAN           string   `json:"wan"`
	AcceptGateway bool     `json:"accept_gateway"`
	PanelPort     int      `json:"panel_port"`
	Domestic      string   `json:"domestic"` // upstream lines
	Foreign       string   `json:"foreign"`
	FakeIPRange   string   `json:"fakeip_range"`
	MixedPort     int      `json:"mixed_port"`
	Nodes         string   `json:"nodes"` // share links, Clash YAML or sing-box JSON; may be empty
}

const (
	defaultDomestic = "udp://223.5.5.5\nhttps://dns.alidns.com/dns-query 223.5.5.5"
	defaultForeign  = "https://dns.google/dns-query 8.8.8.8\nhttps://1.1.1.1/dns-query"
	defaultFakeIP   = "28.0.0.0/8"
	defaultMixed    = 7890
	defaultPanel    = 80
)

// fixedPorts are the ports atom itself uses (deploy-nft.md 2, design 6).
var fixedPorts = map[int]string{53: "DNS", 6666: "FakeIP DNS 入站", 7892: "redirect 入站", 7893: "TProxy 入站", 9090: "Clash API"}

// FieldError is one wrong field.
type FieldError struct {
	Field string `json:"field"`
	Msg   string `json:"msg"`
}

func normalizeLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, strings.Join(strings.Fields(l), " "))
		}
	}
	return strings.Join(out, "\n")
}

// Validate checks the wizard answers. ifaces are the usable interfaces; busy reports why a port is taken
// ("" when it is free).
func (c *Config) Validate(ifaces []Iface, installerPort int, busy func(port int) string) []FieldError {
	var errs []FieldError
	add := func(field, format string, a ...any) {
		errs = append(errs, FieldError{field, fmt.Sprintf(format, a...)})
	}
	have := map[string]bool{}
	for _, i := range ifaces {
		have[i.Name] = true
	}
	switch c.Topology {
	case "bypass":
		if len(c.LAN) != 1 {
			add("lan", "旁路由选一块网卡")
		}
	case "gateway":
		if len(c.LAN) == 0 {
			add("lan", "至少选一块 LAN 网卡")
		}
		if c.WAN == "" {
			add("wan", "选择 WAN 网卡")
		}
		if !c.AcceptGateway {
			add("accept_gateway", "需要确认")
		}
	default:
		add("topology", "选择拓扑")
	}
	for _, n := range c.LAN {
		if !have[n] {
			add("lan", "没有网卡 %s", n)
		}
		if n == c.WAN {
			add("wan", "WAN 不能同时是 LAN")
		}
	}
	if c.Topology == "gateway" && c.WAN != "" && !have[c.WAN] {
		add("wan", "没有网卡 %s", c.WAN)
	}

	portOK := func(field string, port int) bool {
		if port < 1 || port > 65535 {
			add(field, "端口范围 1–65535")
			return false
		}
		if what, ok := fixedPorts[port]; ok {
			add(field, "%d 被 %s 占用", port, what)
			return false
		}
		if port == installerPort {
			add(field, "%d 是安装器自己的端口", port)
			return false
		}
		if who := busy(port); who != "" {
			add(field, "%d 已被 %s 占用", port, who)
			return false
		}
		return true
	}
	if portOK("panel_port", c.PanelPort) && c.PanelPort == c.MixedPort {
		add("panel_port", "不能和 mixed 端口相同")
	}
	portOK("mixed_port", c.MixedPort)

	if _, err := parseUpstreams(c.Domestic, false, "d"); err != nil {
		add("domestic", "%v", err)
	}
	if _, err := parseUpstreams(c.Foreign, true, "f"); err != nil {
		add("foreign", "%v", err)
	}
	if err := checkFakeIPRange(c.FakeIPRange); err != nil {
		add("fakeip_range", "%v", err)
	}
	return errs
}

// checkFakeIPRange mirrors atom's rule: an IPv4 network with prefix 8 to 24, host bits zero, not loopback.
func checkFakeIPRange(s string) error {
	ip, n, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("需要 IPv4 网段，例如 28.0.0.0/8")
	}
	ones, _ := n.Mask.Size()
	if ones < 8 || ones > 24 {
		return fmt.Errorf("前缀长度 8–24")
	}
	if !ip.Equal(n.IP) {
		return fmt.Errorf("主机位必须为 0（应为 %s）", n.String())
	}
	if ip.IsLoopback() {
		return fmt.Errorf("不能是回环网段")
	}
	return nil
}

// Upstream is one entry of PUT /upstreams/{tag}.
type Upstream struct {
	Tag      string `json:"tag"`
	Enabled  bool   `json:"enabled"`
	Protocol string `json:"protocol"`
	Addr     string `json:"addr"`
	DialAddr string `json:"dial_addr,omitempty"`
	UseProxy bool   `json:"use_proxy,omitempty"`
}

var schemeProtocol = map[string]string{"udp": "udp", "tcp": "tcp", "tls": "tls", "https": "https"}

// parseUpstreams reads "addr [dial_addr]" lines. A bare IP is UDP. Foreign non-UDP upstreams go through
// the proxy, like the configuration's own.
func parseUpstreams(text string, foreign bool, tagPrefix string) ([]Upstream, error) {
	var out []Upstream
	for i, line := range strings.Split(normalizeLines(text), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) > 2 {
			return nil, fmt.Errorf("第 %d 行：格式是「地址 [拨号IP]」", i+1)
		}
		addr := f[0]
		scheme := "udp"
		if s, _, ok := strings.Cut(addr, "://"); ok {
			scheme = s
		} else {
			addr = "udp://" + addr
		}
		proto, ok := schemeProtocol[scheme]
		if !ok {
			return nil, fmt.Errorf("第 %d 行：不支持 %s://（udp、tcp、tls、https）", i+1, scheme)
		}
		if strings.TrimPrefix(addr, scheme+"://") == "" {
			return nil, fmt.Errorf("第 %d 行：地址为空", i+1)
		}
		u := Upstream{Tag: fmt.Sprintf("%s%d", tagPrefix, len(out)+1), Enabled: true, Protocol: proto, Addr: addr, UseProxy: foreign && proto != "udp"}
		if len(f) == 2 {
			if net.ParseIP(f[1]) == nil {
				return nil, fmt.Errorf("第 %d 行：拨号地址要是 IP", i+1)
			}
			u.DialAddr = f[1]
		}
		out = append(out, u)
	}
	return out, nil
}

// nodeFormat guesses the import format of pasted nodes.
func nodeFormat(s string) string {
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "{") || strings.HasPrefix(t, "["):
		return "json"
	case strings.HasPrefix(t, "proxies:") || strings.Contains(t, "\nproxies:") || strings.HasPrefix(t, "- name:") || strings.HasPrefix(t, "- {"):
		return "yaml"
	}
	return "link"
}
