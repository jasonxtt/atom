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
	LANGateway    string   `json:"lan_gateway"` // gateway only: the router on the LAN side, for clients not on the LAN
	ClientNets    string   `json:"client_nets"` // gateway only: those clients' networks, one per line
	ReplyVia      bool     `json:"reply_via"`   // bypass only: answers to the LAN go back through the main router
	Router        string   `json:"router"`      // bypass only, with ReplyVia: the main router
	PanelPort     int      `json:"panel_port"`
	Domestic      string   `json:"domestic"` // upstream lines
	Foreign       string   `json:"foreign"`
	FakeIPRange   string   `json:"fakeip_range"`
	MixedPort     int      `json:"mixed_port"`
	Nodes         string   `json:"nodes"` // share links, Clash YAML or sing-box JSON; may be empty

	// keepData is set by the server, not the client: the data of an earlier installation is there, and its
	// settings, rules and nodes win over the DNS and sing-box answers.
	keepData bool
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
		} else if c.ReplyVia {
			if msg := checkRouter(c.Router, c.LAN[0], ifaces); msg != "" {
				add("router", "%s", msg)
			}
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
	if c.Topology == "gateway" {
		_, rerrs := clientRoutes(c.LAN, c.LANGateway, c.ClientNets, c.FakeIPRange, ifaces)
		errs = append(errs, rerrs...)
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
	if portOK("panel_port", c.PanelPort) && !c.keepData && c.PanelPort == c.MixedPort {
		add("panel_port", "不能和 mixed 端口相同")
	}
	if !c.keepData {
		portOK("mixed_port", c.MixedPort)
	}

	if c.keepData {
		return errs
	}
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

// checkRouter checks the main router a single-NIC bypass sends its answers through: an IPv4 address in a
// network of the LAN interface, not one of its own.
func checkRouter(router, lan string, ifaces []Iface) string {
	ip := net.ParseIP(strings.TrimSpace(router))
	if ip == nil || ip.To4() == nil {
		return "需要 IPv4 地址"
	}
	for _, i := range ifaces {
		if i.Name != lan {
			continue
		}
		for _, a := range i.Addrs {
			own, n, err := net.ParseCIDR(a)
			if err != nil {
				continue
			}
			if own.Equal(ip) {
				return "这是本机的地址"
			}
			if n.Contains(ip) {
				return ""
			}
		}
	}
	return fmt.Sprintf("%s 不在网卡 %s 的网段内", strings.TrimSpace(router), lan)
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

// ClientRoute is a route the dual-NIC gateway needs to answer clients that are not on the LAN itself: their
// network via the router on the LAN side. Without it the answers (DNS, and what sing-box sends back) follow
// the default route out of the WAN.
type ClientRoute struct {
	Net string `json:"net"`
	Via string `json:"via"`
	Dev string `json:"dev"`
}

// clientRoutes checks the LAN gateway and the client networks of the wizard and turns them into routes. Both
// empty is fine: the clients are on the LAN.
func clientRoutes(lan []string, gwText, netsText, fakeip string, ifaces []Iface) ([]ClientRoute, []FieldError) {
	var errs []FieldError
	add := func(field, format string, a ...any) {
		errs = append(errs, FieldError{field, fmt.Sprintf(format, a...)})
	}
	gwText = strings.TrimSpace(gwText)
	lines := strings.Fields(normalizeLines(netsText))
	if gwText == "" && len(lines) == 0 {
		return nil, nil
	}
	// the LAN networks the machine is on
	type onLink struct {
		dev string
		net *net.IPNet
	}
	var links []onLink
	isLAN := map[string]bool{}
	for _, n := range lan {
		isLAN[n] = true
	}
	for _, i := range ifaces {
		if !isLAN[i.Name] {
			continue
		}
		for _, a := range i.Addrs {
			if _, n, err := net.ParseCIDR(a); err == nil && n.IP.To4() != nil {
				links = append(links, onLink{i.Name, n})
			}
		}
	}
	dev := ""
	gw := net.ParseIP(gwText)
	switch {
	case gwText == "":
		add("lan_gateway", "填写 LAN 侧网关")
	case gw == nil || gw.To4() == nil:
		add("lan_gateway", "需要 IPv4 地址")
	default:
		for _, l := range links {
			if l.net.Contains(gw) {
				dev = l.dev
				break
			}
		}
		if dev == "" {
			add("lan_gateway", "%s 不在 LAN 网卡的网段内", gwText)
		}
	}
	if len(lines) == 0 {
		add("client_nets", "填写客户端网段，或清空网关")
	}
	_, fake, _ := net.ParseCIDR(strings.TrimSpace(fakeip))
	var out []ClientRoute
	seen := map[string]bool{}
	for _, l := range lines {
		ip, n, err := net.ParseCIDR(l)
		if err != nil || ip.To4() == nil {
			add("client_nets", "%s：需要 IPv4 网段，例如 10.0.0.0/24", l)
			continue
		}
		ones, _ := n.Mask.Size()
		if ones == 0 {
			add("client_nets", "%s：不能是默认路由", l)
			continue
		}
		if !ip.Equal(n.IP) {
			add("client_nets", "%s：主机位必须为 0（应为 %s）", l, n)
			continue
		}
		direct := false
		for _, k := range links {
			if k.net.Contains(n.IP) {
				direct = true
			}
		}
		if direct {
			add("client_nets", "%s：和 LAN 在同一网段，不用填", l)
			continue
		}
		if fake != nil && (fake.Contains(n.IP) || n.Contains(fake.IP)) {
			add("client_nets", "%s：和 FakeIP 网段重叠", l)
			continue
		}
		if seen[n.String()] {
			continue
		}
		seen[n.String()] = true
		out = append(out, ClientRoute{Net: n.String(), Via: gwText, Dev: dev})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}
