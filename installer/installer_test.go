package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderNft(t *testing.T) {
	got, err := renderNft([]string{"ens34", "ens35"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Every IPv4 TCP and UDP from the LAN, not for this machine, goes to sing-box: no sets to pick from.
	for _, want := range []string{
		`iifname != { "ens34", "ens35" } return`,
		"fib daddr type { local, broadcast, multicast } return",
		"meta nfproto ipv4 meta l4proto tcp redirect to :7892\n",
		"ct direction reply return\n\t\tmeta nfproto ipv4 meta l4proto udp tproxy ip to :7893 meta mark set 0x233 accept\n",
		"delete table inet atom",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("nft rules lack %q", want)
		}
	}
	for _, unwanted := range []string{"set fakeip4", "set iprule4", "@fakeip4", "@iprule4", "ip daddr"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("nft rules still pick destinations: %q", unwanted)
		}
	}
	if _, err := renderNft([]string{`eth0" }; flush ruleset; #`}, false); err == nil {
		t.Error("an interface name that is not a plain name must be refused")
	}
	if _, err := renderNft(nil, false); err == nil {
		t.Error("no interface must be refused")
	}
}

func TestRenderGatewayNft(t *testing.T) {
	got, err := renderGatewayNft([]string{"ens34"}, "ens33", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `iifname { "ens34" } oifname "ens33" masquerade`) {
		t.Errorf("unexpected NAT rule:\n%s", got)
	}
	if !strings.Contains(got, `iifname "ens33" tcp dport 80 drop`) {
		t.Errorf("the panel must be closed on the WAN:\n%s", got)
	}
	if _, err := renderGatewayNft([]string{"ens34"}, `a" b`, 80); err == nil {
		t.Error("a bad WAN name must be refused")
	}
}

func TestRenderUnits(t *testing.T) {
	p := Paths{Root: "/opt/atom", UnitDir: "/etc/systemd/system"}
	bypass := renderDNSUnit(p, "/usr/sbin/nft", "/usr/sbin/sysctl", false, "")
	if strings.Contains(bypass, "atom-gateway.nft") || strings.Contains(bypass, "ip_forward") {
		t.Errorf("the single-NIC unit must not touch NAT or ip_forward:\n%s", bypass)
	}
	for _, want := range []string{"ExecStartPre=/usr/sbin/nft -f /opt/atom/etc/atom.nft", "ExecStartPre=/opt/atom/lib/policy-route.sh", "ExecStart=/opt/atom/bin/atomdns start -d /opt/atom/data -c config_custom.yaml"} {
		if !strings.Contains(bypass, want) {
			t.Errorf("DNS unit lacks %q", want)
		}
	}
	gw := renderDNSUnit(p, "/usr/sbin/nft", "/usr/sbin/sysctl", true, "")
	for _, want := range []string{"nft -f /opt/atom/etc/atom-gateway.nft", "sysctl -w net.ipv4.ip_forward=1"} {
		if !strings.Contains(gw, want) {
			t.Errorf("gateway unit lacks %q", want)
		}
	}
	if strings.Index(gw, "atom.nft") > strings.Index(gw, "policy-route.sh") {
		t.Error("rules are loaded before the policy routing")
	}
	sb := renderSingBoxUnit(p)
	for _, want := range []string{"ExecReload=/bin/kill -HUP $MAINPID", "Restart=on-failure", "ConditionPathExists=/opt/atom/data/sing-box/config.json", "/opt/atom/bin/sing-box run -D /opt/atom/data/sing-box"} {
		if !strings.Contains(sb, want) {
			t.Errorf("sing-box unit lacks %q", want)
		}
	}
}

func TestSetPanelPort(t *testing.T) {
	in := "log:\n  level: warn\n\n#comment\napi:\n  http: \"0.0.0.0:9099\"\n\ninclude:\n  - \"a.yaml\"\n"
	out, err := setPanelPort(in, 80)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `http: "0.0.0.0:80"`) || strings.Contains(out, "9099") || !strings.Contains(out, "include:") {
		t.Errorf("unexpected result:\n%s", out)
	}
	if _, err := setPanelPort("log:\n  level: warn\n", 80); err == nil {
		t.Error("a configuration without api.http must be refused")
	}
	// the shipped configuration, when this checkout has it
	if data, err := os.ReadFile("../atomdns/config/config_custom.yaml"); err == nil {
		if _, err := setPanelPort(string(data), 8080); err != nil {
			t.Errorf("the shipped config_custom.yaml: %v", err)
		}
	}
}

func TestParseUpstreams(t *testing.T) {
	ups, err := parseUpstreams("# c\n223.5.5.5\nhttps://dns.alidns.com/dns-query 223.5.5.5\n\ntcp://1.1.1.1\ntls://dns.google 8.8.8.8", false, "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 4 {
		t.Fatalf("%d upstreams", len(ups))
	}
	if ups[0].Addr != "udp://223.5.5.5" || ups[0].Protocol != "udp" || ups[0].Tag != "d1" {
		t.Errorf("bare IP: %+v", ups[0])
	}
	if ups[1].Protocol != "https" || ups[1].DialAddr != "223.5.5.5" {
		t.Errorf("DoH: %+v", ups[1])
	}
	if ups[2].Protocol != "tcp" || ups[3].Protocol != "tls" || ups[0].UseProxy || ups[1].UseProxy {
		t.Errorf("domestic upstreams must not use the proxy: %+v", ups)
	}
	fo, err := parseUpstreams(defaultForeign+"\n8.8.4.4", true, "f")
	if err != nil {
		t.Fatal(err)
	}
	if !fo[0].UseProxy || !fo[1].UseProxy || fo[2].UseProxy {
		t.Errorf("foreign non-UDP upstreams go through the proxy, UDP cannot: %+v", fo)
	}
	for _, bad := range []string{"quic://1.1.1.1", "https://a b c", "https://x 1.1.1", "udp://"} {
		if _, err := parseUpstreams(bad, false, "d"); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if d, _ := parseUpstreams(defaultDomestic, false, "d"); len(d) != 2 {
		t.Errorf("the default domestic list: %+v", d)
	}
}

func TestDefaultUpstreamsMatchShippedConfig(t *testing.T) {
	for file, want := range map[string][]string{
		"../atomdns/config/sub_config/forward_local.yaml": {"223.5.5.5", "dns.alidns.com/dns-query"},
		"../atomdns/config/sub_config/forward_nocn.yaml":  {"dns.google/dns-query", "1.1.1.1/dns-query"},
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Skip("no atomdns checkout next to the installer")
		}
		for _, w := range want {
			if !strings.Contains(string(data), w) {
				t.Errorf("%s no longer lists %s: update the wizard's default upstreams", file, w)
			}
		}
	}
}

func TestCheckFakeIPRange(t *testing.T) {
	for _, ok := range []string{"28.0.0.0/8", "198.18.0.0/15", "10.9.0.0/24"} {
		if err := checkFakeIPRange(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "28.0.0.1/8", "28.0.0.0/7", "28.0.0.0/25", "127.0.0.0/8", "fd00::/8", "abc"} {
		if checkFakeIPRange(bad) == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestValidatePanelLogin(t *testing.T) {
	ifs := []Iface{{Name: "eth0"}}
	free := func(int) string { return "" }
	base := Config{Topology: "bypass", LAN: []string{"eth0"}, PanelPort: 80, Domestic: defaultDomestic, Foreign: defaultForeign, FakeIPRange: defaultFakeIP, MixedPort: 7890}
	for _, tc := range []struct {
		user, password, field string
	}{
		{"", "", ""},
		{"admin", "pw", ""},
		{" ", "pw", "panel_user"},
		{"admin", "", "panel_password"},
		{"a\tb", "pw", "panel_user"},
		{strings.Repeat("u", 65), "pw", "panel_user"},
		{"admin", strings.Repeat("p", 257), "panel_password"},
	} {
		c := base
		c.PanelUser, c.PanelPassword = tc.user, tc.password
		errs := c.Validate(ifs, 9996, free)
		got := ""
		if len(errs) > 0 {
			got = errs[0].Field
		}
		if len(errs) > 1 || got != tc.field {
			t.Errorf("%q/%q: %+v, want %q", tc.user, tc.password, errs, tc.field)
		}
	}
}

func TestAPIClientSendsTheLoginKey(t *testing.T) {
	data := t.TempDir()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	api := newAPIClient(0, data)
	api.base = srv.URL
	if err := api.call("GET", "/status", nil, nil); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(data, "webinfo"), 0o755)
	os.WriteFile(filepath.Join(data, "webinfo", "atom_auth.json"), []byte(`{"username":"u","salt":"s","hash":"h","key":"K1"}`), 0o600)
	if err := api.call("GET", "/status", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "" || got[1] != "Bearer K1" {
		t.Errorf("headers: %q", got)
	}
}

func TestValidate(t *testing.T) {
	ifs := []Iface{{Name: "eth0"}, {Name: "eth1"}}
	free := func(int) string { return "" }
	base := Config{Topology: "bypass", LAN: []string{"eth0"}, PanelPort: 80, Domestic: defaultDomestic, Foreign: defaultForeign, FakeIPRange: defaultFakeIP, MixedPort: 7890}
	if errs := base.Validate(ifs, 9996, free); len(errs) != 0 {
		t.Fatalf("a good configuration: %+v", errs)
	}
	fields := func(c Config, busy func(int) string) map[string]bool {
		out := map[string]bool{}
		for _, e := range c.Validate(ifs, 9996, busy) {
			out[e.Field] = true
		}
		return out
	}
	c := base
	c.Topology = "gateway"
	if f := fields(c, free); !f["wan"] || !f["accept_gateway"] {
		t.Errorf("a gateway needs a WAN and the confirmation: %v", f)
	}
	c.WAN, c.AcceptGateway = "eth1", true
	if f := fields(c, free); len(f) != 0 {
		t.Errorf("a good gateway: %v", f)
	}
	c.WAN = "eth0"
	if f := fields(c, free); !f["wan"] {
		t.Errorf("the WAN cannot be a LAN: %v", f)
	}
	c = base
	c.LAN = []string{"nope"}
	if f := fields(c, free); !f["lan"] {
		t.Errorf("an unknown NIC: %v", f)
	}
	c = base
	c.PanelPort = 53
	if f := fields(c, free); !f["panel_port"] {
		t.Errorf("port 53: %v", f)
	}
	c = base
	c.PanelPort = 9996
	if f := fields(c, free); !f["panel_port"] {
		t.Errorf("the installer's own port: %v", f)
	}
	c = base
	c.MixedPort = 80
	if f := fields(c, free); !f["panel_port"] {
		t.Errorf("the same port twice: %v", f)
	}
	c = base
	if f := fields(c, func(p int) string {
		if p == 80 {
			return "nginx"
		}
		return ""
	}); !f["panel_port"] {
		t.Errorf("a busy panel port: %v", f)
	}
	c = base
	c.Domestic, c.FakeIPRange = "gopher://x", "1.2.3.4/8"
	if f := fields(c, free); !f["domestic"] || !f["fakeip_range"] {
		t.Errorf("bad upstreams and range: %v", f)
	}
}

func TestNodeFormat(t *testing.T) {
	for in, want := range map[string]string{
		"vless://a@b:1?x=y#n":               "link",
		"ss://abc#n\ntrojan://x@y:1":        "link",
		"{\"type\":\"socks\"}":              "json",
		"[{\"type\":\"socks\"}]":            "json",
		"proxies:\n  - name: a":             "yaml",
		"port: 7890\nproxies:\n  - name: a": "yaml",
		"- name: a\n  type: ss":             "yaml",
	} {
		if got := nodeFormat(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

const ssSample = `udp   UNCONN 0      0                            127.0.0.54:53        0.0.0.0:*    users:(("systemd-resolve",pid=278,fd=19))
udp   UNCONN 0      0                         127.0.0.53%lo:53        0.0.0.0:*    users:(("systemd-resolve",pid=278,fd=17))
tcp   LISTEN 0      4096                         127.0.0.54:53        0.0.0.0:*    users:(("systemd-resolve",pid=278,fd=20))
tcp   LISTEN 0      128                             0.0.0.0:22        0.0.0.0:*    users:(("sshd",pid=495,fd=6))
tcp   LISTEN 0      1024                                  *:53               *:*    users:(("atomdns",pid=900,fd=11))
udp   UNCONN 0      0      [fe80::be24:11ff:fe4d:d4cb]%eth0:546          [::]:*
tcp   LISTEN 0      128                                [::]:22           [::]:*    users:(("sshd",pid=495,fd=7))
`

func TestParseSSAndOccupants(t *testing.T) {
	ls := parseSS(ssSample)
	if len(ls) != 7 {
		t.Fatalf("%d listeners: %+v", len(ls), ls)
	}
	if ls[1].Addr != "127.0.0.53%lo" || ls[1].Port != 53 || ls[1].Proc != "systemd-resolve" || ls[1].PID != 278 || ls[1].Proto != "udp" {
		t.Errorf("%+v", ls[1])
	}
	if ls[4].Addr != "*" || ls[4].Port != 53 {
		t.Errorf("wildcard: %+v", ls[4])
	}
	if ls[6].Addr != "::" || ls[6].Port != 22 {
		t.Errorf("v6: %+v", ls[6])
	}
	if ls[5].PID != 0 || ls[5].Port != 546 {
		t.Errorf("no process: %+v", ls[5])
	}
	unit := func(pid int) string {
		switch pid {
		case 278:
			return "systemd-resolved.service"
		case 900:
			return dnsUnitName
		}
		return "ssh.service"
	}
	occ := occupantsOf(ls, 53, unit, false)
	if len(occ) != 1 || occ[0].Kind != "resolved" || occ[0].PID != 278 || len(occ[0].Addrs) != 3 {
		t.Errorf("port 53 (atom's own listener does not count): %+v", occ)
	}
	occ = occupantsOf(ls, 22, unit, false)
	if len(occ) != 1 || occ[0].Kind != "unit" || occ[0].Unit != "ssh.service" {
		t.Errorf("port 22: %+v", occ)
	}
	if occ := occupantsOf(ls, 80, unit, false); len(occ) != 0 {
		t.Errorf("port 80: %+v", occ)
	}
	if occ := occupantsOf(ls, 546, unit, false); len(occ) != 1 || occ[0].Kind != "" {
		t.Errorf("a socket without a process cannot be handled: %+v", occ)
	}
}

func TestOSAndRoutes(t *testing.T) {
	deb13 := parseOSRelease("PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\n")
	if p := checkOS(deb13); p != "" {
		t.Error(p)
	}
	for _, bad := range []string{"ID=debian\nVERSION_ID=\"11\"", "ID=ubuntu\nVERSION_ID=\"20.04\"", "ID=fedora\nVERSION_ID=\"40\""} {
		if checkOS(parseOSRelease(bad)) == "" {
			t.Errorf("%q must be refused", bad)
		}
	}
	if checkOS(parseOSRelease("ID=ubuntu\nVERSION_ID=\"22.04\"")) != "" {
		t.Error("Ubuntu 22.04 is supported")
	}
	routes := "Iface\tDestination\tGateway\tFlags\nens18\t00000000\t0100000A\t0003\nens18\t0000000A\t00000000\t0001\ndocker0\t000011AC\t00000000\t0001\n"
	def := parseDefaultRoutes(routes)
	if !def["ens18"] || def["docker0"] || len(def) != 1 {
		t.Errorf("%v", def)
	}
	for name, virt := range map[string]bool{"ens18": false, "eth0": false, "docker0": true, "veth1234": true, "br-ab12": true, "tailscale0": true, "wg0": true} {
		if isVirtualName(name) != virt {
			t.Errorf("%s: virtual=%v", name, !virt)
		}
	}
}

func TestPrivateRemote(t *testing.T) {
	for addr, want := range map[string]bool{
		"192.168.1.5:5000": true, "10.0.0.86:1": true, "172.16.3.4:1": true, "127.0.0.1:1": true, "[::1]:1": true, "[fd00::1]:1": true,
		"[::ffff:192.168.1.5]:1": true, "8.8.8.8:1": false, "172.32.0.1:1": false, "[2001:db8::1]:1": false, "garbage": false,
	} {
		if got := isPrivateRemote(addr); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"0.1.0", "0.1.0", 0}, {"0.2.0", "0.1.9", 1}, {"0.1.0", "0.1.1", -1}, {"v1.0.0", "0.9.9", 1}, {"0.10.0", "0.9.0", 1}, {"0.1.0-rc1", "0.1.0", 0}} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s: %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestRefreshData(t *testing.T) {
	pkg, data := t.TempDir(), t.TempDir()
	write := func(root, rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(root, rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}
	write(pkg, "config_custom.yaml", "api:\n  http: \"0.0.0.0:9099\"\n")
	write(pkg, "sub_config/a.yaml", "new-a")
	write(pkg, "rule/atom_catalog.json", "new-catalog")
	write(pkg, "rule/geosite_cn.txt", "shipped-geosite")
	write(pkg, "webinfo/audit_settings.json", "shipped-audit")
	write(pkg, "README.md", "doc")
	write(pkg, "cache/.gitkeep", "")

	if err := refreshData(pkg, data, 80, ""); err != nil {
		t.Fatal(err)
	}
	if got := read(data, "config_custom.yaml"); !strings.Contains(got, "0.0.0.0:80") {
		t.Errorf("port not set: %s", got)
	}
	if read(data, "README.md") != "<missing>" {
		t.Error("the configuration's README is not installed")
	}
	if _, err := os.Stat(filepath.Join(data, "cache")); err != nil {
		t.Error("empty directories are created")
	}

	// the user changed things, a daily update replaced a list; then the package is applied again
	write(data, "webinfo/audit_settings.json", "user-audit")
	write(data, "webinfo/atom_rules.json", "user-rules")
	write(data, "rule/geosite_cn.txt", "updated-geosite")
	write(data, "sub_config/a.yaml", "old-a")
	write(data, "sub_config/stale.yaml", "stale")
	write(pkg, "sub_config/a.yaml", "newer-a")
	write(pkg, "rule/atom_catalog.json", "newer-catalog")
	write(pkg, "rule/new_list.txt", "brand-new")
	if err := refreshData(pkg, data, 8080, ""); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"sub_config/a.yaml": "newer-a", "rule/atom_catalog.json": "newer-catalog", // product files: replaced
		"webinfo/audit_settings.json": "user-audit", "webinfo/atom_rules.json": "user-rules", "rule/geosite_cn.txt": "updated-geosite", // data: kept
		"rule/new_list.txt": "brand-new", // new data files arrive
	} {
		if got := read(data, rel); got != want {
			t.Errorf("%s: %q, want %q", rel, got, want)
		}
	}
	if got := read(data, "config_custom.yaml"); !strings.Contains(got, "0.0.0.0:8080") {
		t.Errorf("port not updated: %s", got)
	}
}

func makeTarGz(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.tar.gz")
	f, _ := os.Create(path)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	f.Close()
	return path
}

func TestInstallWebUI(t *testing.T) {
	data := t.TempDir()
	if err := installWebUI(makeTarGz(t, map[string]string{"index.html": "v1", "assets/app.js": "js1"}), data); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "ui/main/index.html")); string(b) != "v1" {
		t.Errorf("%q", b)
	}
	if err := installWebUI(makeTarGz(t, map[string]string{"index.html": "v2"}), data); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "ui/main/index.html")); string(b) != "v2" {
		t.Errorf("%q", b)
	}
	if _, err := os.Stat(filepath.Join(data, "ui/main/assets")); err == nil {
		t.Error("the old files must be gone")
	}
	if err := installWebUI(makeTarGz(t, map[string]string{"other.txt": "x"}), data); err == nil {
		t.Error("a package without index.html must be refused")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "ui/main/index.html")); string(b) != "v2" {
		t.Error("a refused package leaves the installed UI alone")
	}
	if err := extractTarGz(makeTarGz(t, map[string]string{"../evil": "x"}), t.TempDir()); err == nil {
		t.Error("a path that leaves the directory must be refused")
	}
}

func TestJobView(t *testing.T) {
	j := newJob([]string{"a", "b"})
	j.Logf("one")
	j.Logf("two")
	j.setStep(0, "ok")
	j.warn("careful")
	v := j.view(0)
	if len(v.Log) != 3 || v.Next != 3 || v.Steps[0].Status != "ok" || len(v.Warnings) != 1 || v.Done {
		t.Errorf("%+v", v)
	}
	if v := j.view(2); len(v.Log) != 1 || v.Log[0] != "! careful" {
		t.Errorf("%+v", v)
	}
	j.finish(nil, 80)
	if v := j.view(3); !v.Done || !v.OK || v.PanelPort != 80 || len(v.Log) != 0 {
		t.Errorf("%+v", v)
	}
}

func TestStateAndChangesRoundTrip(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	if _, err := loadState(p); err == nil {
		t.Error("no state yet")
	}
	st := &InstallState{Version: "0.1.0", Complete: true, Topology: "gateway", LAN: []string{"a"}, WAN: "b", PanelPort: 80}
	if err := writeJSON(p.State(), st); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(p)
	if err != nil || got.Version != "0.1.0" || !got.Complete || got.WAN != "b" {
		t.Errorf("%+v %v", got, err)
	}
	ch := loadChanges(p)
	ch.IPForwardBefore = "0"
	if err := saveChanges(p, ch); err != nil {
		t.Fatal(err)
	}
	if loadChanges(p).IPForwardBefore != "0" {
		t.Error("changes not kept")
	}
}

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"0.1.0-1", "0.1.0-2", "0.1.0-3", "0.1.0-4", "0.1.0-5"} {
		os.MkdirAll(filepath.Join(dir, n), 0o755)
	}
	pruneBackups(dir, 3)
	e, _ := os.ReadDir(dir)
	if len(e) != 3 || e[0].Name() != "0.1.0-3" {
		t.Errorf("%v", e)
	}
}

func TestStateJSONHasNoNulls(t *testing.T) {
	s := newServer(&Installer{Paths: Paths{Root: t.TempDir()}})
	data, err := json.Marshal(s.state())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"problems", "ifaces", "addrs", "port53"} {
		if strings.Contains(string(data), `"`+field+`":null`) {
			t.Errorf("%s is null: the wizard page calls array methods on it", field)
		}
	}
}

func TestWizardRefusesOtherPagesAndHosts(t *testing.T) {
	s := newServer(&Installer{Paths: Paths{Root: t.TempDir()}})
	h := s.handler()
	host, _ := os.Hostname()
	do := func(method, target, ctype, origin, hostHeader string) int {
		var body io.Reader
		if method == "POST" {
			body = strings.NewReader("{}")
		}
		req := httptest.NewRequest(method, target, body)
		req.RemoteAddr = "192.168.1.5:4000"
		if hostHeader != "" {
			req.Host = hostHeader
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("GET", "/api/state", "", "", "192.168.1.9:9996"); c != 200 {
		t.Errorf("a plain GET by address: %d", c)
	}
	if c := do("GET", "/", "", "http://192.168.1.9:9996", "192.168.1.9:9996"); c != 200 {
		t.Errorf("the page's own origin: %d", c)
	}
	if c := do("GET", "/api/state", "", "", "evil.example:9996"); c != 403 {
		t.Errorf("a rebinding host name: %d", c)
	}
	if host != "" {
		if c := do("GET", "/api/state", "", "", host+":9996"); c != 200 {
			t.Errorf("this machine's own name: %d", c)
		}
	}
	if c := do("POST", "/api/validate", "text/plain", "", "192.168.1.9:9996"); c != 403 {
		t.Errorf("a POST that is not JSON (a cross-site form or no-cors fetch): %d", c)
	}
	if c := do("POST", "/api/validate", "application/json", "https://evil.example", "192.168.1.9:9996"); c != 403 {
		t.Errorf("a POST from another origin: %d", c)
	}
	if c := do("POST", "/api/validate", "application/json", "http://192.168.1.9:9996", "192.168.1.9:9996"); c != 200 {
		t.Errorf("the page's own POST: %d", c)
	}
}

func TestRouteConflict(t *testing.T) {
	ours := "9233:\tfrom all fwmark 0x233 lookup 233\n"
	if c := routeConflict("0:\tfrom all lookup local\n32766:\tfrom all lookup main\n", ""); c != "" {
		t.Errorf("an empty table is free: %q", c)
	}
	if c := routeConflict("0:\tfrom all lookup local\n"+ours, "local default dev lo scope host \n"); c != "" {
		t.Errorf("what an earlier try left is not a conflict: %q", c)
	}
	if c := routeConflict("100:\tfrom 10.9.0.0/16 lookup 233\n", ""); c == "" {
		t.Error("another rule that uses table 233 is a conflict")
	}
	if c := routeConflict("", "default via 10.0.0.1 dev eth1\n"); c == "" {
		t.Error("another route in table 233 is a conflict")
	}
	if r := tableRoutes("Error: ipv4: FIB table does not exist.\nDump terminated", errors.New("exit status 2")); routeConflict("", r) != "" {
		t.Errorf("a table that was never created is free: %q", r)
	}
}

func TestFinalize(t *testing.T) {
	ins := &Installer{Paths: Paths{Root: t.TempDir()}, Version: "0.1.0"}
	if err := ins.finalize(); err == nil {
		t.Error("nothing was installed: finalize must fail")
	}
	if hasData(ins.Paths) {
		t.Error("a failed finalize leaves no data marker")
	}
	if err := writeJSON(ins.Paths.State(), &InstallState{Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	if err := ins.finalize(); err != nil {
		t.Fatal(err)
	}
	st, _ := loadState(ins.Paths)
	if !st.Complete || !hasData(ins.Paths) {
		t.Errorf("complete=%v data=%v", st.Complete, hasData(ins.Paths))
	}
}

func TestMakeBackupCoversEverythingTheUpgradeOverwrites(t *testing.T) {
	root := t.TempDir()
	unitDir := t.TempDir()
	p := Paths{Root: root, UnitDir: unitDir}
	ins := &Installer{Paths: p}
	files := map[string]string{
		filepath.Join(root, "bin/atomdns"):                  "bin",
		filepath.Join(root, "etc/install.json"):             "state",
		filepath.Join(root, "lib/policy-route.sh"):          "route",
		filepath.Join(root, "data/sub_config/a.yaml"):       "sub",
		filepath.Join(root, "data/ui/main/index.html"):      "ui",
		filepath.Join(root, "data/config_custom.yaml"):      "cfg",
		filepath.Join(root, "data/rule/atom_catalog.json"):  "catalog",
		filepath.Join(unitDir, dnsUnitName):                 "unit1",
		filepath.Join(unitDir, singBoxUnitName):             "unit2",
		filepath.Join(root, "data/webinfo/atom_rules.json"): "user data",
	}
	for path, content := range files {
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	dir := filepath.Join(root, "backup/x")
	if err := ins.makeBackup(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bin/atomdns", "etc/install.json", "lib/policy-route.sh", "data/sub_config/a.yaml", "data/ui/main/index.html", "data/config_custom.yaml", "data/rule/atom_catalog.json", "units/" + dnsUnitName, "units/" + singBoxUnitName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is not in the backup", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "data/webinfo")); err == nil {
		t.Error("the user's data is not part of the backup")
	}
}

func TestFinishAndInstallDoNotRace(t *testing.T) {
	s := newServer(&Installer{Paths: Paths{Root: t.TempDir()}})
	h := s.handler()
	post := func(path string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		req.RemoteAddr = "127.0.0.1:5000"
		req.Host = "127.0.0.1:9996"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	s.job = newJob([]string{"a"}) // a running installation
	if c := post("/api/finish"); c != 409 {
		t.Errorf("finish while an installation runs: %d", c)
	}
	if c := post("/api/free"); c != 409 {
		t.Errorf("a free action while an installation runs: %d", c)
	}
	s.job.finish(nil, 80)
	if c := post("/api/finish"); c != 200 {
		t.Errorf("finish after the job: %d", c)
	}
	if c := post("/api/install"); c != 409 {
		t.Errorf("an installation after finish was accepted: %d", c)
	}
	if c := post("/api/free"); c != 409 {
		t.Errorf("a free action after finish was accepted: %d", c)
	}
}

type recLogger struct{ lines []string }

func (r *recLogger) Logf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recLogger) ran(sub string) bool {
	for _, l := range r.lines {
		if strings.HasPrefix(l, "$ ") && strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestUninstallFollowsTheOwnershipRecord(t *testing.T) {
	newPaths := func() Paths { return Paths{Root: t.TempDir(), UnitDir: t.TempDir()} }

	// the network part was rolled back (or never installed): table 233 and the nft tables are not touched
	p := newPaths()
	if err := writeJSON(p.State(), &InstallState{Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	if err := saveChanges(p, &Changes{Network: false}); err != nil {
		t.Fatal(err)
	}
	log := &recLogger{}
	if err := uninstall(p, false, log); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"nft", "ip route flush", "ip rule", "disable --now"} {
		if log.ran(sub) {
			t.Errorf("a rolled back install must not run %q on uninstall: %v", sub, log.lines)
		}
	}

	// the network part is the installer's: it is torn down, and then no longer claimed
	p = newPaths()
	writeJSON(p.State(), &InstallState{Version: "0.1.0"})
	saveChanges(p, &Changes{Network: true})
	log = &recLogger{}
	uninstall(p, true, log)
	for _, sub := range []string{"nft delete table inet atom", "ip route flush table 233", "ip rule del fwmark 0x233"} {
		if !log.ran(sub) {
			t.Errorf("an owned network must be torn down with %q: %v", sub, log.lines)
		}
	}

	// only changes.json (a service was stopped, the install never got to write install.json): nothing of the
	// network is touched and the service is restored
	p = newPaths()
	saveChanges(p, &Changes{DisabledUnits: []UnitState{{Unit: "x-test.service"}}})
	if !hasRecords(p) {
		t.Fatal("changes.json alone is a record")
	}
	log = &recLogger{}
	if err := uninstall(p, false, log); err != nil {
		t.Fatal(err)
	}
	if log.ran("nft") || log.ran("ip route") || log.ran("daemon-reload") {
		t.Errorf("without install.json the network and the units are left alone: %v", log.lines)
	}
	if hasRecords(newPaths()) {
		t.Error("no records, no uninstall")
	}
}

func TestTeardownClearsOwnership(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	saveChanges(p, &Changes{Network: true, IPForwardBefore: ""})
	teardownNetwork(p, &recLogger{})
	if loadChanges(p).Network {
		t.Error("after a teardown the network is no longer the installer's")
	}
	log := &recLogger{}
	teardownNetwork(p, log) // a second teardown must not touch anything
	if log.ran("nft") || log.ran("ip ") {
		t.Errorf("%v", log.lines)
	}
}

func TestOneWayToClose(t *testing.T) {
	s := newServer(&Installer{Paths: Paths{Root: t.TempDir()}})
	s.job = newJob([]string{"a"})
	if s.requestClose() {
		t.Error("a running installation refuses to close")
	}
	s.mu.Lock()
	s.seen = time.Now().Add(-2 * idleLimit)
	s.mu.Unlock()
	if s.idleClose() {
		t.Error("the idle timer must not end a running installation")
	}
	s.job.finish(nil, 80)
	s.mu.Lock()
	s.seen = time.Now().Add(-2 * afterDoneLimit)
	s.mu.Unlock()
	if !s.idleClose() {
		t.Error("a finished installation that nobody uses closes")
	}
	if !s.closing {
		t.Error("closing must be set before the wizard exits")
	}
	if !s.busyOrClosing() {
		t.Error("nothing is accepted once closing")
	}

	s2 := newServer(&Installer{Paths: Paths{Root: t.TempDir()}})
	if s2.idleClose() {
		t.Error("a wizard that was just used does not close")
	}
	s2.mu.Lock()
	s2.seen = time.Now().Add(-2 * idleLimit)
	s2.mu.Unlock()
	if !s2.idleClose() {
		t.Error("an unused wizard closes after the idle limit")
	}
}

func TestClientRoutes(t *testing.T) {
	ifaces := []Iface{{Name: "ens33", Addrs: []string{"10.0.2.4/24"}}, {Name: "ens34", Addrs: []string{"10.0.3.4/24"}, Default: true}}
	lan := []string{"ens33"}
	if r, errs := clientRoutes(lan, "", "", "28.0.0.0/8", ifaces); r != nil || errs != nil {
		t.Errorf("both empty: clients on the LAN, no routes: %v %v", r, errs)
	}
	r, errs := clientRoutes(lan, "10.0.2.2", "10.0.0.0/24\n# a comment\n10.0.1.0/24\n10.0.0.0/24", "28.0.0.0/8", ifaces)
	if errs != nil || len(r) != 2 || r[0] != (ClientRoute{"10.0.0.0/24", "10.0.2.2", "ens33"}) || r[1].Net != "10.0.1.0/24" {
		t.Errorf("routes: %v %v", r, errs)
	}
	field := func(errs []FieldError, f string) string {
		for _, e := range errs {
			if e.Field == f {
				return e.Msg
			}
		}
		return ""
	}
	for _, c := range []struct{ gw, nets, field string }{
		{"10.0.2.2", "", "client_nets"},            // a gateway without networks
		{"", "10.0.0.0/24", "lan_gateway"},         // networks without a gateway
		{"10.0.3.1", "10.0.0.0/24", "lan_gateway"}, // the gateway is on the WAN side
		{"fe80::1", "10.0.0.0/24", "lan_gateway"},
		{"10.0.2.2", "10.0.2.0/24", "client_nets"}, // already on the LAN
		{"10.0.2.2", "10.0.0.5/24", "client_nets"}, // host bits
		{"10.0.2.2", "0.0.0.0/0", "client_nets"},
		{"10.0.2.2", "28.1.0.0/16", "client_nets"}, // inside the FakeIP range
		{"10.0.2.2", "10.0.0.0/24 10.1.0.0/16 foo", "client_nets"},
	} {
		if _, errs := clientRoutes(lan, c.gw, c.nets, "28.0.0.0/8", ifaces); field(errs, c.field) == "" {
			t.Errorf("%q %q: want an error on %s, got %v", c.gw, c.nets, c.field, errs)
		}
	}
	cfg := Config{Topology: "gateway", LAN: lan, WAN: "ens34", AcceptGateway: true, LANGateway: "10.0.3.1", ClientNets: "10.0.0.0/24", PanelPort: 80, Domestic: defaultDomestic, Foreign: defaultForeign, FakeIPRange: defaultFakeIP, MixedPort: 7890}
	if field(cfg.Validate(ifaces, 9996, func(int) string { return "" }), "lan_gateway") == "" {
		t.Error("Validate must check the client routes of a gateway")
	}
	cfg.Topology, cfg.LAN = "bypass", []string{"ens33"}
	if errs := cfg.Validate(ifaces, 9996, func(int) string { return "" }); len(errs) != 0 {
		t.Errorf("a bypass installation ignores them: %v", errs)
	}
}

func TestParseViaRoutes(t *testing.T) {
	text := "default via 10.0.3.1 dev ens34 onlink \n10.0.0.0/24 via 10.0.2.2 dev ens33 src 10.0.2.4 \n10.0.2.0/24 dev ens33 proto kernel scope link src 10.0.2.4 \n10.9.9.9 via 10.0.2.2 dev ens33\n"
	got := parseViaRoutes(text)
	if len(got) != 1 || got[0] != (ClientRoute{"10.0.0.0/24", "10.0.2.2", "ens33"}) {
		t.Errorf("%v", got)
	}
}

func TestPolicyRouteWithClientRoutes(t *testing.T) {
	s, err := renderPolicyRoute([]ClientRoute{{"10.0.0.0/24", "10.0.2.2", "ens33"}}, nil)
	if err != nil || !strings.Contains(s, "ip route replace 10.0.0.0/24 via 10.0.2.2 dev ens33\n") || !strings.Contains(s, "lookup "+routeTable) {
		t.Errorf("%v\n%s", err, s)
	}
	for _, bad := range []ClientRoute{{"10.0.0.0/24; reboot", "10.0.2.2", "ens33"}, {"10.0.0.1/24", "10.0.2.2", "ens33"}, {"10.0.0.0/24", "x", "ens33"}, {"10.0.0.0/24", "10.0.2.2", "ens33 && x"}} {
		if _, err := renderPolicyRoute([]ClientRoute{bad}, nil); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}

func TestOwnRoutes(t *testing.T) {
	a := ClientRoute{"10.0.0.0/24", "10.0.2.2", "ens33"}
	b := ClientRoute{"10.0.1.0/24", "10.0.2.2", "ens33"}
	table := map[string]string{}
	existing := func(r ClientRoute) string { return table[r.Net] }
	log := &recLogger{}
	// the machine already has a: it stays the machine's; b is new and the installer's
	table[a.Net] = "10.0.0.0/24 via 10.0.2.2 dev ens33 src 10.0.2.4 "
	got, err := ownRoutes(nil, []ClientRoute{a, b}, existing, log)
	if err != nil || len(got) != 1 || got[0] != b {
		t.Errorf("%v %v", got, err)
	}
	// b, added by the installer before, is in the table now and still the installer's
	table[b.Net] = "10.0.1.0/24 via 10.0.2.2 dev ens33 "
	if got, err := ownRoutes([]ClientRoute{b}, []ClientRoute{a, b}, existing, log); err != nil || len(got) != 1 || got[0] != b {
		t.Errorf("%v %v", got, err)
	}
	// a route for the network through another gateway is a conflict
	table[a.Net] = "10.0.0.0/24 via 10.0.2.9 dev ens33 "
	if _, err := ownRoutes(nil, []ClientRoute{a}, existing, log); err == nil {
		t.Error("another gateway for the same network must be refused")
	}
}

// The gateway warning says what the topology needs (the main router sends this machine what is to be
// proxied), not that all LAN traffic passes through it.
func TestGatewayWarning(t *testing.T) {
	page := string(indexHTML)
	if !strings.Contains(page, "主路由需要把 FakeIP 网段和要代理的网段转到这台机器") {
		t.Error("the gateway warning lacks what the main router must send here")
	}
	if strings.Contains(page, "全部流量都会经过这台机器") {
		t.Error("the old gateway warning is still there")
	}
}

func TestReplyViaRouter(t *testing.T) {
	off, _ := renderNft([]string{"eth0"}, false)
	if strings.Contains(off, "ct mark") || strings.Contains(off, "output_reply") {
		t.Errorf("without the option nothing is marked:\n%s", off)
	}
	on, err := renderNft([]string{"eth0"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"meta l4proto tcp ct mark set 0x234 redirect to :7892\n",
		"meta l4proto udp ct mark set 0x234 tproxy ip to :7893 meta mark set 0x233 accept\n",
		"type route hook output priority mangle; policy accept;\n\t\tct direction reply ct mark 0x234 meta mark set 0x234\n",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("nft rules lack %q:\n%s", want, on)
		}
	}
	pr, err := renderPolicyRoute(nil, &ClientRoute{"0.0.0.0/0", "192.168.1.1", "eth0"})
	if err != nil || !strings.Contains(pr, "ip rule add fwmark 0x234 lookup 234 priority 9234\n") || !strings.Contains(pr, "ip route replace default via 192.168.1.1 dev eth0 table 234\n") {
		t.Errorf("%v\n%s", err, pr)
	}
	if _, err := renderPolicyRoute(nil, &ClientRoute{"0.0.0.0/0", "192.168.1.1; reboot", "eth0"}); err == nil {
		t.Error("a bad router must be refused")
	}
	p := Paths{Root: "/opt/atom", UnitDir: "/etc/systemd/system"}
	u := renderDNSUnit(p, "/usr/sbin/nft", "/usr/sbin/sysctl", false, "eth0.10")
	if !strings.Contains(u, "ExecStartPre=/usr/sbin/sysctl -w net/ipv4/conf/all/accept_redirects=0 net/ipv4/conf/eth0.10/accept_redirects=0\n") {
		t.Errorf("redirects must be off:\n%s", u)
	}

	ifs := []Iface{{Name: "eth0", Addrs: []string{"192.168.1.2/24"}}}
	for router, ok := range map[string]bool{"192.168.1.1": true, "192.168.1.2": false, "10.0.0.1": false, "": false, "x": false, "fe80::1": false} {
		if got := checkRouter(router, "eth0", ifs) == ""; got != ok {
			t.Errorf("router %q: ok=%v", router, got)
		}
	}
	c := Config{Topology: "bypass", LAN: []string{"eth0"}, PanelPort: 80, Domestic: defaultDomestic, Foreign: defaultForeign, FakeIPRange: defaultFakeIP, MixedPort: 7890, ReplyVia: true}
	hasRouter := false
	for _, e := range c.Validate(ifs, 9996, func(int) string { return "" }) {
		hasRouter = hasRouter || e.Field == "router"
	}
	if !hasRouter {
		t.Error("the option needs the router")
	}
	c.Router = "192.168.1.1"
	if errs := c.Validate(ifs, 9996, func(int) string { return "" }); len(errs) != 0 {
		t.Errorf("%v", errs)
	}

	if c := replyConflict("9234:\tfrom all fwmark 0x234 lookup 234\n", "", false); c != "" {
		t.Errorf("our own rule is no conflict: %s", c)
	}
	for _, other := range []string{"100:\tfrom 10.9.0.0/16 lookup 234\n", "9234:\tfrom 10.0.0.0/8 fwmark 0x234 lookup 234\n", "200:\tfrom all fwmark 0x234 lookup main\n"} {
		if c := replyConflict(other, "", false); c == "" {
			t.Errorf("%q is a conflict", other)
		}
	}
	if replyConflict("", "10.0.0.0/24 via 10.0.0.1 dev eth1\n", true) == "" {
		t.Error("a route the installer did not add is a conflict")
	}
	if replyConflict("", "default via 10.0.0.1 dev eth1\n", false) == "" || replyConflict("", "default via 10.0.0.1 dev eth1\n", true) != "" {
		t.Error("routes in table 234 are a conflict unless the installer added them")
	}
	if got := parseDefaultVia("default via 192.168.1.1 dev eth0 proto dhcp \n10.0.0.0/24 via 10.0.2.2 dev ens33\n"); got["eth0"] != "192.168.1.1" || len(got) != 1 {
		t.Errorf("%v", got)
	}
}

func TestCheckArch(t *testing.T) {
	for _, a := range []string{"amd64", "arm64"} {
		if p := checkArch(a); p != "" {
			t.Errorf("%s refused: %s", a, p)
		}
	}
	for _, a := range []string{"arm", "386", "riscv64"} {
		if checkArch(a) == "" {
			t.Errorf("%s accepted", a)
		}
	}
}

func TestRenderOpenRCServices(t *testing.T) {
	p := Paths{Root: "/opt/atom", OpenRC: true}
	dns := renderDNSInit(p, "/usr/sbin/nft", "/sbin/sysctl", false, "")
	for _, want := range []string{"#!/sbin/openrc-run", "supervisor=supervise-daemon", "respawn_max=0", "command=/opt/atom/bin/atomdns",
		`command_args="start -d /opt/atom/data -c config_custom.yaml"`, "need net", "/var/log/atom/atom-dns.log", "start_pre()",
		"/usr/sbin/nft -f /opt/atom/etc/atom.nft || return 1", "/opt/atom/lib/policy-route.sh || return 1"} {
		if !strings.Contains(dns, want) {
			t.Errorf("DNS init script lacks %q:\n%s", want, dns)
		}
	}
	if strings.Contains(dns, "ip_forward") || strings.Contains(dns, "atom-gateway.nft") {
		t.Error("single NIC script must not touch NAT or ip_forward")
	}
	gw := renderDNSInit(p, "/usr/sbin/nft", "/sbin/sysctl", true, "eth0")
	if !strings.Contains(gw, "/sbin/sysctl -w net.ipv4.ip_forward=1") || !strings.Contains(gw, "atom-gateway.nft") || !strings.Contains(gw, "conf/eth0/accept_redirects=0") {
		t.Errorf("gateway script:\n%s", gw)
	}
	if strings.Index(gw, "atom.nft") > strings.Index(gw, "policy-route.sh") {
		t.Error("rules are loaded before the policy routing")
	}
	sb := renderSingBoxInit(p)
	for _, want := range []string{`extra_started_commands="reload"`, "--signal HUP", "supervise-daemon", "/opt/atom/data/sing-box/config.json ] ||", "run -D /opt/atom/data/sing-box -c /opt/atom/data/sing-box/config.json"} {
		if !strings.Contains(sb, want) {
			t.Errorf("sing-box init script lacks %q:\n%s", want, sb)
		}
	}
	if p.DNSUnit() != "/etc/init.d/atom-dns" && (Paths{UnitDir: "/etc/init.d", OpenRC: true}).DNSUnit() != "/etc/init.d/atom-dns" {
		t.Error("OpenRC script name")
	}
	if (Paths{UnitDir: "/u"}).SingBoxUnit() != "/u/atom-sing-box.service" {
		t.Error("systemd unit name")
	}
}

func TestSetReloadCommand(t *testing.T) {
	in := "# reload: [x]\nsingbox:\n    enabled: true\n        reload: [systemctl, reload-or-restart, atom-sing-box.service]\n        other: [a]\n"
	out, err := setReloadCommand(in, "/opt/atom/lib/reload-sing-box.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "        reload: [/opt/atom/lib/reload-sing-box.sh]\n") || !strings.Contains(out, "# reload: [x]") || !strings.Contains(out, "other: [a]") {
		t.Errorf("%s", out)
	}
	if _, err := setReloadCommand("nothing: 1\n", "x"); err == nil {
		t.Error("a file without a reload line must fail")
	}
	shipped, err := os.ReadFile("../atomdns/config/sub_config/atom_rules.yaml")
	if err == nil {
		if _, err := setReloadCommand(string(shipped), "/x"); err != nil {
			t.Errorf("the shipped atom_rules.yaml: %v", err)
		}
	}
	// refreshData applies it to atom_rules.yaml only, and only when asked
	pkg, data := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(pkg, "sub_config"), 0o755)
	os.WriteFile(filepath.Join(pkg, "sub_config", "atom_rules.yaml"), []byte(in), 0o644)
	os.WriteFile(filepath.Join(pkg, "config_custom.yaml"), []byte("api:\n  http: \"0.0.0.0:80\"\n"), 0o644)
	if err := refreshData(pkg, data, 80, "/r.sh"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(data, "sub_config", "atom_rules.yaml"))
	if !strings.Contains(string(got), "reload: [/r.sh]") {
		t.Errorf("%s", got)
	}
	if err := refreshData(pkg, data, 80, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(data, "sub_config", "atom_rules.yaml"))
	if !strings.Contains(string(got), "systemctl, reload-or-restart") {
		t.Errorf("without a reload command the file is copied as it is: %s", got)
	}
}

func TestOpenRCServiceOfPID(t *testing.T) {
	root := t.TempDir()
	daemons, proc := filepath.Join(root, "daemons"), filepath.Join(root, "proc")
	pidfile := func(name string, pid int) {
		os.MkdirAll(filepath.Join(daemons, name), 0o755)
		pf := filepath.Join(root, name+".pid")
		os.WriteFile(pf, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
		os.WriteFile(filepath.Join(daemons, name, "001"), []byte("exec=/x\nargv_0=/x\npidfile="+pf+"\n"), 0o644)
	}
	stat := func(pid, ppid int, comm string) {
		os.MkdirAll(filepath.Join(proc, fmt.Sprint(pid)), 0o755)
		os.WriteFile(filepath.Join(proc, fmt.Sprint(pid), "stat"), []byte(fmt.Sprintf("%d (%s) S %d 1 1 0\n", pid, comm, ppid)), 0o644)
	}
	pidfile("atomdns", 500)    // the pidfile holds the process itself
	pidfile("supervised", 600) // the pidfile holds a supervisor, the listener is its child
	stat(500, 1, "atomdns")
	stat(600, 1, "supervise-daemon")
	stat(601, 600, "a (b) c") // comm with spaces and parentheses
	stat(602, 601, "worker")
	stat(700, 1, "stray")
	for pid, want := range map[int]string{500: "atomdns", 602: "supervised", 601: "supervised", 700: "", 999: ""} {
		if got := openrcServiceOf(daemons, proc, pid); got != want {
			t.Errorf("pid %d: %q, want %q", pid, got, want)
		}
	}
	// the Kind of such an occupant is openrc; our own services do not count
	ls := []Listener{{Proto: "udp", Addr: "0.0.0.0:53", Port: 53, PID: 500, Proc: "atomdns"}, {Proto: "udp", Addr: "*:53", Port: 53, PID: 602, Proc: "x"}}
	occ := occupantsOf(ls, 53, func(pid int) string { return openrcServiceOf(daemons, proc, pid) }, true)
	if len(occ) != 2 || occ[0].Kind != "openrc" || occ[0].Unit != "atomdns" {
		t.Errorf("%+v", occ)
	}
	occ = occupantsOf(ls, 53, func(pid int) string { return "atom-dns" }, true)
	if len(occ) != 0 {
		t.Errorf("our own service is not an occupant: %+v", occ)
	}
}

func TestResolvConfAndRunlevels(t *testing.T) {
	for text, want := range map[string]bool{
		"nameserver 127.0.0.1\n":                       true,
		"# c\nnameserver ::1\nnameserver 127.0.0.53\n": true,
		"nameserver 127.0.0.1\nnameserver 10.0.0.3\n":  false,
		"nameserver 10.0.0.3\n":                        false,
		"search lan\n":                                 false,
		"":                                             false,
	} {
		if loopbackOnly(text) != want {
			t.Errorf("%q: loopbackOnly = %v", text, !want)
		}
	}
	rl := t.TempDir()
	for _, d := range []string{"default/atomdns", "boot/atomdns", "default/crond", "sysinit/dev"} {
		os.MkdirAll(filepath.Dir(filepath.Join(rl, d)), 0o755)
		os.Symlink("/etc/init.d/x", filepath.Join(rl, d))
	}
	if got := runlevelsOf(rl, "atomdns"); strings.Join(got, ",") != "boot,default" {
		t.Errorf("%v", got)
	}
	if got := runlevelsOf(rl, "nothing"); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

func TestFreeOpenRCServiceAndResolvConfRestore(t *testing.T) {
	root, conf := t.TempDir(), filepath.Join(t.TempDir(), "resolv.conf")
	resolvConfPath = conf
	defer func() { resolvConfPath = "/etc/resolv.conf" }()
	p := Paths{Root: root, OpenRC: true}
	if err := freeOpenRCService(p, "atom-dns", &recLogger{}); err == nil {
		t.Error("our own service must not be freed")
	}
	if err := freeOpenRCService(p, "a/b", &recLogger{}); err == nil {
		t.Error("a path is not a service name")
	}
	// the saved resolv.conf comes back, and the backup goes
	if err := writeFileAtomic(p.Etc("resolv.conf.orig"), []byte("nameserver 127.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(conf, []byte("nameserver 10.0.0.1\n"), 0o644)
	ch := &Changes{ResolvConfSaved: true}
	if err := restoreResolvConf(p, ch, &recLogger{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(conf); string(got) != "nameserver 127.0.0.1\n" || ch.ResolvConfSaved {
		t.Errorf("resolv.conf %q, saved=%v", got, ch.ResolvConfSaved)
	}
	if _, err := os.Stat(p.Etc("resolv.conf.orig")); err == nil {
		t.Error("the backup is removed once restored")
	}
	if err := restoreResolvConf(p, ch, &recLogger{}); err != nil {
		t.Errorf("nothing saved, nothing to do: %v", err)
	}
}

func TestMissingDeps(t *testing.T) {
	has := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, x := range names {
				if x == n {
					return true
				}
			}
			return false
		}
	}
	if got := missingDeps(true, false, has("ip"), has()); strings.Join(got, ",") != "nftables,iproute2" {
		t.Errorf("Alpine: the busybox ip does not count: %v", got)
	}
	if got := missingDeps(true, true, has("nft"), has("iproute2")); len(got) != 0 {
		t.Errorf("Alpine with everything: %v", got)
	}
	if got := missingDeps(false, true, has(), has()); strings.Join(got, ",") != "nftables,iproute2,procps" {
		t.Errorf("Debian: %v", got)
	}
	if got := missingDeps(false, false, has("nft", "ip"), has()); len(got) != 0 {
		t.Errorf("Debian: %v", got)
	}
}

func TestOldChangesFileStillReads(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	os.MkdirAll(p.Etc(""), 0o755)
	os.WriteFile(p.Changes(), []byte(`{"apt_installed": ["nftables"], "pkg_installed": ["iproute2"]}`), 0o644)
	ch := loadChanges(p)
	if strings.Join(ch.PkgInstalled, ",") != "nftables,iproute2" || len(ch.AptInstalled) != 0 {
		t.Errorf("%+v", ch)
	}
	saveChanges(p, ch)
	data, _ := os.ReadFile(p.Changes())
	if strings.Contains(string(data), "apt_installed") {
		t.Errorf("the old field is not written again: %s", data)
	}
}

func TestAlpineAndContainer(t *testing.T) {
	for _, v := range []string{"3.19.0", "3.23.5", "4.0"} {
		if p := checkOS(parseOSRelease("ID=alpine\nVERSION_ID=" + v)); p != "" {
			t.Errorf("Alpine %s: %s", v, p)
		}
	}
	for _, v := range []string{"3.18.9", "3.9", "2.7"} {
		if checkOS(parseOSRelease("ID=alpine\nVERSION_ID="+v)) == "" {
			t.Errorf("Alpine %s must be refused", v)
		}
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, ".dockerenv")
	if inContainer([]string{marker}, []byte("PATH=/bin\x00HOME=/root\x00")) {
		t.Error("not a container")
	}
	if !inContainer([]string{marker}, []byte("PATH=/bin\x00container=lxc\x00")) {
		t.Error("container= in PID 1's environment")
	}
	os.WriteFile(marker, nil, 0o644)
	if !inContainer([]string{marker}, nil) {
		t.Error("marker file")
	}
}

func TestUninstallRemovesOpenRCLogs(t *testing.T) {
	logs := filepath.Join(t.TempDir(), "atom")
	openRCLogDir = logs
	defer func() { openRCLogDir = "/var/log/atom" }()
	for _, purge := range []bool{false, true} {
		os.MkdirAll(logs, 0o755)
		os.WriteFile(filepath.Join(logs, "atom-dns.log"), []byte("x"), 0o644)
		p := Paths{Root: t.TempDir(), UnitDir: t.TempDir(), OpenRC: true}
		writeJSON(p.State(), &InstallState{Version: "0.1.0"})
		if err := uninstall(p, purge, &recLogger{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(logs); err == nil {
			t.Errorf("purge=%v: the log directory stays", purge)
		}
	}
}

func TestConfigFromState(t *testing.T) {
	st := &InstallState{Topology: "gateway", LAN: []string{"eth1"}, WAN: "eth0", PanelPort: 8080,
		ClientRoutes: []ClientRoute{{"10.1.0.0/24", "192.168.1.2", "eth1"}, {"10.2.0.0/24", "192.168.1.2", "eth1"}}}
	c := configFromState(st)
	if c.Topology != "gateway" || c.WAN != "eth0" || c.PanelPort != 8080 || c.LANGateway != "192.168.1.2" || c.ClientNets != "10.1.0.0/24\n10.2.0.0/24" || !c.AcceptGateway {
		t.Errorf("%+v", c)
	}
	c = configFromState(&InstallState{Topology: "bypass", LAN: []string{"eth0"}, ReplyVia: "10.0.0.1", PanelPort: 80})
	if !c.ReplyVia || c.Router != "10.0.0.1" || c.LANGateway != "" || c.AcceptGateway {
		t.Errorf("%+v", c)
	}
	if c := configFromState(&InstallState{Topology: "bypass"}); c.LAN == nil {
		t.Error("LAN must not be null in JSON")
	}
}

func TestReconfigureLoginValidation(t *testing.T) {
	ifs := []Iface{{Name: "eth0", Addrs: []string{"10.0.0.5/24"}}}
	base := func() Config {
		return Config{Topology: "bypass", LAN: []string{"eth0"}, PanelPort: 8081, keepData: true, currentUser: "admin"}
	}
	has := func(c Config, field string) bool {
		for _, e := range c.Validate(ifs, 9996, func(int) string { return "" }) {
			if e.Field == field {
				return true
			}
		}
		return false
	}
	c := base()
	c.PanelUser = "admin"
	if has(c, "panel_password") {
		t.Error("the same user without a password changes nothing")
	}
	c.PanelUser = "root"
	if !has(c, "panel_password") {
		t.Error("a new user needs a password")
	}
	c = base()
	c.PanelLoginOff = true
	c.PanelUser = "x"
	if has(c, "panel_password") || has(c, "panel_user") {
		t.Error("turning the login off ignores the fields")
	}
	c = base()
	c.currentUser = ""
	c.PanelUser = "admin"
	if !has(c, "panel_password") {
		t.Error("without a login a user needs a password")
	}
}

func TestSetLogin(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	api := &apiClient{base: srv.URL + "/api/v1/atom", http: srv.Client(), authFile: filepath.Join(t.TempDir(), "none")}
	j := newJob(nil)
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{PanelLoginOff: true}, "DELETE /api/v1/atom/auth"},
		{Config{PanelUser: "a", PanelPassword: "b"}, "PUT /api/v1/atom/auth"},
		{Config{PanelUser: "a"}, ""},
		{Config{}, ""},
	} {
		calls = nil
		setLogin(api, c.cfg, j)
		if got := strings.Join(calls, ","); got != c.want {
			t.Errorf("%+v: %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestReconfigureUndoesWhatTheOldStateLacks(t *testing.T) {
	route := ClientRoute{"10.1.0.0/24", "192.168.1.2", "eth1"}
	kept := ClientRoute{"10.2.0.0/24", "192.168.1.2", "eth1"}
	newCh := &Changes{IPForwardBefore: "0", Reply: true, Routes: []ClientRoute{route, kept}}
	rc := &reconfigureRun{oldSt: InstallState{Topology: "bypass"}, oldCh: Changes{Routes: []ClientRoute{kept}}}
	log := &recLogger{}
	rc.undoAdditions(newCh, log)
	for _, want := range []string{"nft delete table ip atom_nat", "ip rule del fwmark 0x234", "ip route del 10.1.0.0/24"} {
		if !log.ran(want) {
			t.Errorf("not undone: %q: %v", want, log.lines)
		}
	}
	if log.ran("ip route del 10.2.0.0/24") {
		t.Error("a route the old state had stays")
	}
	// the old state was a gateway with the reply option: nothing of it is taken away
	rc = &reconfigureRun{oldSt: InstallState{Topology: "gateway", ReplyVia: "10.0.0.1"}, oldCh: Changes{IPForwardBefore: "0", Routes: []ClientRoute{route, kept}}}
	log = &recLogger{}
	rc.undoAdditions(&Changes{IPForwardBefore: "0", Reply: true, Routes: []ClientRoute{route, kept}}, log)
	if len(log.lines) != 0 {
		t.Errorf("%v", log.lines)
	}
}

func TestBeginReconfigureBacksUp(t *testing.T) {
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir()}
	ins := &Installer{Paths: p}
	if _, err := ins.beginReconfigure(&recLogger{}); err == nil {
		t.Error("nothing installed")
	}
	writeJSON(p.State(), &InstallState{Version: "0.1.0", Complete: true, Topology: "bypass", PanelPort: 80})
	os.WriteFile(p.DNSUnit(), []byte("unit"), 0o644)
	os.MkdirAll(p.Lib(""), 0o755)
	os.WriteFile(p.Lib("policy-route.sh"), []byte("#!/bin/sh"), 0o755)
	rc, err := ins.beginReconfigure(&recLogger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"etc/install.json", "lib/policy-route.sh", "units/atom-dns.service"} {
		if _, err := os.Stat(filepath.Join(rc.dir, f)); err != nil {
			t.Errorf("%s is not in the backup", f)
		}
	}
	if rc.oldSt.PanelPort != 80 {
		t.Errorf("%+v", rc.oldSt)
	}
}

func TestOwnRoutesChangedGateway(t *testing.T) {
	old := ClientRoute{"10.1.0.0/24", "192.168.1.2", "eth1"}
	now := ClientRoute{"10.1.0.0/24", "192.168.1.3", "eth1"}
	existing := func(ClientRoute) string { return "10.1.0.0/24 via 192.168.1.2 dev eth1 " }
	log := &recLogger{}
	got, err := ownRoutes([]ClientRoute{old}, []ClientRoute{now}, existing, log)
	if err != nil || len(got) != 1 || got[0] != now {
		t.Fatalf("%v %v", got, err)
	}
	if !log.ran("ip route del 10.1.0.0/24 via 192.168.1.2") {
		t.Errorf("the old route goes: %v", log.lines)
	}
	if _, err := ownRoutes(nil, []ClientRoute{now}, existing, log); err == nil {
		t.Error("a route the installer did not add is still a conflict")
	}
}

func TestBackupKeepsThePanelLoginPrivate(t *testing.T) {
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir()}
	ins := &Installer{Paths: p}
	auth := filepath.Join(p.Data(), "webinfo", "atom_auth.json")
	os.MkdirAll(filepath.Dir(auth), 0o755)
	os.WriteFile(auth, []byte(`{"key":"k"}`), 0o600)
	dir := filepath.Join(p.Backup(), "b")
	if err := ins.makeBackup(dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "data/webinfo/atom_auth.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	// a login the failed run created is removed; the old one comes back
	os.WriteFile(auth, []byte(`{"key":"new"}`), 0o600)
	os.MkdirAll(filepath.Join(p.Data(), "sub_config"), 0o755)
	for _, it := range ins.backupItems() {
		if it.name == "data/webinfo/atom_auth.json" && (!it.file || it.mode != 0o600) {
			t.Errorf("%+v", it)
		}
	}
}
