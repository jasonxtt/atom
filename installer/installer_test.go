package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderNft(t *testing.T) {
	got, err := renderNft([]string{"ens34", "ens35"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`iifname != { "ens34", "ens35" } return`, "redirect to :7892", "tproxy ip to :7893 meta mark set 0x233 accept", "set fakeip4", "set iprule4", "delete table inet atom"} {
		if !strings.Contains(got, want) {
			t.Errorf("nft rules lack %q", want)
		}
	}
	if _, err := renderNft([]string{`eth0" }; flush ruleset; #`}); err == nil {
		t.Error("an interface name that is not a plain name must be refused")
	}
	if _, err := renderNft(nil); err == nil {
		t.Error("no interface must be refused")
	}
}

func TestRenderGatewayNft(t *testing.T) {
	got, err := renderGatewayNft([]string{"ens34"}, "ens33")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `iifname { "ens34" } oifname "ens33" masquerade`) {
		t.Errorf("unexpected NAT rule:\n%s", got)
	}
	if _, err := renderGatewayNft([]string{"ens34"}, `a" b`); err == nil {
		t.Error("a bad WAN name must be refused")
	}
}

func TestRenderUnits(t *testing.T) {
	p := Paths{Root: "/opt/atom", UnitDir: "/etc/systemd/system"}
	bypass := renderDNSUnit(p, "/usr/sbin/nft", "/usr/sbin/sysctl", false)
	if strings.Contains(bypass, "atom-gateway.nft") || strings.Contains(bypass, "ip_forward") {
		t.Errorf("the single-NIC unit must not touch NAT or ip_forward:\n%s", bypass)
	}
	for _, want := range []string{"ExecStartPre=/usr/sbin/nft -f /opt/atom/etc/atom.nft", "ExecStartPre=/opt/atom/lib/policy-route.sh", "ExecStart=/opt/atom/bin/atomdns start -d /opt/atom/data -c config_custom.yaml"} {
		if !strings.Contains(bypass, want) {
			t.Errorf("DNS unit lacks %q", want)
		}
	}
	gw := renderDNSUnit(p, "/usr/sbin/nft", "/usr/sbin/sysctl", true)
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
	occ := occupantsOf(ls, 53, unit)
	if len(occ) != 1 || occ[0].Kind != "resolved" || occ[0].PID != 278 || len(occ[0].Addrs) != 3 {
		t.Errorf("port 53 (atom's own listener does not count): %+v", occ)
	}
	occ = occupantsOf(ls, 22, unit)
	if len(occ) != 1 || occ[0].Kind != "unit" || occ[0].Unit != "ssh.service" {
		t.Errorf("port 22: %+v", occ)
	}
	if occ := occupantsOf(ls, 80, unit); len(occ) != 0 {
		t.Errorf("port 80: %+v", occ)
	}
	if occ := occupantsOf(ls, 546, unit); len(occ) != 1 || occ[0].Kind != "" {
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

	if err := refreshData(pkg, data, 80); err != nil {
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
	if err := refreshData(pkg, data, 8080); err != nil {
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
