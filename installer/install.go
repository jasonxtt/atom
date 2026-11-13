package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Installer holds what every operation needs.
type Installer struct {
	Paths         Paths
	PkgDir        string // the unpacked package
	Version       string
	InstallerPort int
}

// Job is one run of the installation, observed by the wizard page.
type Job struct {
	mu       sync.Mutex
	steps    []stepState
	log      []string
	done     bool
	ok       bool
	errMsg   string
	warnings []string
	panel    int
	hints    jobHints
}

// jobHints is what the finished page tells the user to do next.
type jobHints struct {
	Topology    string `json:"topology"`
	LANIP       string `json:"lan_ip"`
	FakeIPRange string `json:"fakeip_range"`
}

type stepState struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pending | running | ok | fail
}

type JobView struct {
	Steps     []stepState `json:"steps"`
	Log       []string    `json:"log"`
	Next      int         `json:"next"`
	Done      bool        `json:"done"`
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
	PanelPort int         `json:"panel_port,omitempty"`
	Hints     jobHints    `json:"hints"`
}

func newJob(names []string) *Job {
	j := &Job{}
	for _, n := range names {
		j.steps = append(j.steps, stepState{n, "pending"})
	}
	return j
}

func (j *Job) Logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.log) < 4000 {
		j.log = append(j.log, fmt.Sprintf(format, args...))
	}
}

func (j *Job) warn(format string, args ...any) {
	j.mu.Lock()
	j.warnings = append(j.warnings, fmt.Sprintf(format, args...))
	j.mu.Unlock()
	j.Logf("! "+format, args...)
}

func (j *Job) setStep(i int, status string) {
	j.mu.Lock()
	j.steps[i].Status = status
	j.mu.Unlock()
}

func (j *Job) view(since int) JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	if since < 0 || since > len(j.log) {
		since = 0
	}
	return JobView{Steps: append([]stepState(nil), j.steps...), Log: append([]string{}, j.log[since:]...), Next: len(j.log),
		Done: j.done, OK: j.ok, Error: j.errMsg, Warnings: append([]string(nil), j.warnings...), PanelPort: j.panel, Hints: j.hints}
}

func (j *Job) finish(err error, panel int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done = true
	j.ok = err == nil
	if err != nil {
		j.errMsg = err.Error()
	} else {
		j.panel = panel
	}
}

const defaultGroup = "g-default"

// dataMarker is written when an installation completes: the data directory then holds settings, rules and
// nodes that a later installation over it keeps.
func dataMarker(p Paths) string { return filepath.Join(p.Data(), ".atom-data") }

func hasData(p Paths) bool {
	_, err := os.Stat(dataMarker(p))
	return err == nil
}

var installSteps = []string{"依赖", "程序", "配置", "网络", "启动", "设置", "自检"}

// runInstall performs the installation. A failure after the network files were written takes the live
// network changes back (nft tables, policy routing, ip_forward) so the machine is left reachable.
func (ins *Installer) runInstall(cfg Config, j *Job) {
	hints := jobHints{Topology: cfg.Topology, FakeIPRange: cfg.FakeIPRange}
	for _, i := range listInterfaces() {
		if len(cfg.LAN) > 0 && i.Name == cfg.LAN[0] {
			hints.LANIP, _, _ = strings.Cut(i.Addrs[0], "/")
		}
	}
	j.mu.Lock()
	j.hints = hints
	j.mu.Unlock()
	netStarted := false // a refusal before the network step leaves an earlier installation as it is
	err := func() error {
		steps := []func() error{
			func() error { return ins.stepDeps(cfg, j) },
			func() error { return ins.stepBinaries(j) },
			func() error { return ins.stepConfig(cfg, j) },
			func() error {
				if err := ins.checkRouteTable(cfg); err != nil { // nothing has been changed yet: no teardown for a refusal
					return err
				}
				netStarted = true
				return ins.stepNetwork(cfg, j)
			},
			func() error { return ins.stepStart(cfg, j) },
			func() error { return ins.stepSettings(cfg, j) },
			func() error { return ins.stepCheck(cfg, j) },
		}
		for i, fn := range steps {
			j.setStep(i, "running")
			if err := fn(); err != nil {
				j.setStep(i, "fail")
				return fmt.Errorf("%s：%w", installSteps[i], err)
			}
			j.setStep(i, "ok")
		}
		return nil
	}()
	if err == nil {
		err = ins.finalize()
	}
	if err != nil {
		j.Logf("安装失败：%v", err)
		if netStarted {
			ins.rollbackNetwork(j)
		}
		j.finish(err, 0)
		return
	}
	j.finish(nil, cfg.PanelPort)
}

// finalize records that the installation is complete: the data marker first, then the state. A failure here
// is a failure of the installation (the caller rolls the network back), and leaves neither record behind.
func (ins *Installer) finalize() error {
	st, err := loadState(ins.Paths)
	if err != nil {
		return err
	}
	if err := writeJSON(dataMarker(ins.Paths), map[string]string{"version": ins.Version}); err != nil {
		return fmt.Errorf("记录安装结果失败：%w", err)
	}
	st.Complete, st.UpdatedAt = true, time.Now()
	if err := writeJSON(ins.Paths.State(), st); err != nil {
		os.Remove(dataMarker(ins.Paths))
		return fmt.Errorf("记录安装结果失败：%w", err)
	}
	return nil
}

func isAlpine() bool {
	_, err := os.Stat("/etc/alpine-release")
	return err == nil
}

// missingDeps lists the packages to install. On Alpine iproute2 is always wanted: the busybox ip may lack
// policy routing and there is no ss.
func missingDeps(alpine, needSysctl bool, haveBin func(string) bool, havePkg func(string) bool) []string {
	var need []string
	if !haveBin("nft") {
		need = append(need, "nftables")
	}
	if alpine {
		if !havePkg("iproute2") {
			need = append(need, "iproute2")
		}
		return need
	}
	if !haveBin("ip") {
		need = append(need, "iproute2")
	}
	if needSysctl && !haveBin("sysctl") {
		need = append(need, "procps")
	}
	return need
}

// ensureDeps installs what is missing and records it, so that it is known what the installer brought.
func ensureDeps(p Paths, needSysctl bool, j Logger) error {
	alpine := isAlpine()
	need := missingDeps(alpine, needSysctl,
		func(n string) bool { _, err := exec.LookPath(n); return err == nil },
		func(n string) bool { _, err := quietCmd("apk", "info", "-e", n); return err == nil })
	if len(need) == 0 {
		j.Logf("依赖都在")
		return nil
	}
	if alpine {
		if err := runCmd(j, 5*time.Minute, "apk", "update"); err != nil {
			return err
		}
		if err := runCmd(j, 10*time.Minute, "apk", append([]string{"add"}, need...)...); err != nil {
			return err
		}
	} else {
		if err := runCmd(j, 5*time.Minute, "apt-get", "update"); err != nil {
			return err
		}
		if err := runCmd(j, 10*time.Minute, "apt-get", append([]string{"install", "-y", "--no-install-recommends"}, need...)...); err != nil {
			return err
		}
	}
	ch := loadChanges(p)
	for _, n := range need {
		if !slices.Contains(ch.PkgInstalled, n) {
			ch.PkgInstalled = append(ch.PkgInstalled, n)
		}
	}
	return saveChanges(p, ch)
}

func (ins *Installer) stepDeps(cfg Config, j *Job) error {
	return ensureDeps(ins.Paths, cfg.Topology == "gateway" || cfg.ReplyVia, j)
}

// atom-installer first: once anything is installed, the uninstaller is there too.
var installedBinaries = []string{"atom-installer", "atomdns", "sing-box"}

func (ins *Installer) installBinaries(log Logger) error {
	for _, name := range installedBinaries {
		src := filepath.Join(ins.PkgDir, "bin", name)
		if err := copyFileAtomic(src, ins.Paths.Bin(name), 0o755); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		log.Logf("%s → %s", name, ins.Paths.Bin(name))
	}
	return nil
}

func (ins *Installer) stepBinaries(j *Job) error { return ins.installBinaries(j) }

func (ins *Installer) installData(panelPort int, log Logger) error {
	reload := ""
	if ins.Paths.OpenRC {
		reload = ins.Paths.Lib("reload-sing-box.sh")
	}
	if err := refreshData(filepath.Join(ins.PkgDir, "config"), ins.Paths.Data(), panelPort, reload); err != nil {
		return fmt.Errorf("配置: %w", err)
	}
	log.Logf("配置 → %s", ins.Paths.Data())
	if err := installWebUI(filepath.Join(ins.PkgDir, "atom-webui.tar.gz"), ins.Paths.Data()); err != nil {
		return fmt.Errorf("面板: %w", err)
	}
	log.Logf("面板 → %s/ui/main", ins.Paths.Data())
	return nil
}

func (ins *Installer) stepConfig(cfg Config, j *Job) error { return ins.installData(cfg.PanelPort, j) }

// writeNetworkFiles renders the nft rules, the policy-route script and the two units from the state.
func (ins *Installer) writeNetworkFiles(st *InstallState, log Logger) error {
	p := ins.Paths
	nft, err := renderNft(st.LAN, st.ReplyVia != "")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p.Etc("atom.nft"), []byte(nft), 0o644); err != nil {
		return err
	}
	if st.Topology == "gateway" {
		gw, err := renderGatewayNft(st.LAN, st.WAN, st.PanelPort)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(p.Etc("atom-gateway.nft"), []byte(gw), 0o644); err != nil {
			return err
		}
	} else {
		os.Remove(p.Etc("atom-gateway.nft"))
	}
	var reply *ClientRoute
	replyDev := ""
	if st.ReplyVia != "" {
		reply = &ClientRoute{Net: "0.0.0.0/0", Via: st.ReplyVia, Dev: st.LAN[0]}
		replyDev = st.LAN[0]
	}
	pr, err := renderPolicyRoute(st.ClientRoutes, reply)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p.Lib("policy-route.sh"), []byte(pr), 0o755); err != nil {
		return err
	}
	for _, f := range []string{"atom.nft", "atom-gateway.nft"} {
		if _, err := os.Stat(p.Etc(f)); err != nil {
			continue
		}
		if err := runCmd(log, 30*time.Second, st.NftBin, "-c", "-f", p.Etc(f)); err != nil {
			return fmt.Errorf("nft 规则检查没通过: %w", err)
		}
	}
	dnsDef, sbDef := renderDNSUnit, renderSingBoxUnit
	if p.OpenRC {
		dnsDef, sbDef = renderDNSInit, renderSingBoxInit
		if err := writeFileAtomic(p.Lib("reload-sing-box.sh"), []byte(renderReloadScript()), 0o755); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(p.DNSUnit(), []byte(dnsDef(p, st.NftBin, st.SysctlBin, st.Topology == "gateway", replyDev)), p.unitMode()); err != nil {
		return err
	}
	if err := writeFileAtomic(p.SingBoxUnit(), []byte(sbDef(p)), p.unitMode()); err != nil {
		return err
	}
	return p.Init().Reload(log)
}

func lookPathOr(name, fallback string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return fallback
}

// routeConflict says whether policy routing table 233 or its rule is already in use by something else; the
// installer's own rule and route (left by an earlier try) are not a conflict.
func routeConflict(rules, routes string) string {
	for _, l := range strings.Split(rules, "\n") {
		if strings.Contains(l, "lookup "+routeTable) && !strings.Contains(l, "fwmark "+fwmark) {
			return strings.TrimSpace(l)
		}
	}
	for _, l := range strings.Split(routes, "\n") {
		if l = strings.TrimSpace(l); l != "" && l != "local default dev lo scope host" {
			return l
		}
	}
	return ""
}

// tableRoutes is the output of `ip route show table 233`. On a machine where the table was never created
// the command fails with "FIB table does not exist": nothing uses it.
func tableRoutes(out string, err error) string {
	if err != nil && strings.Contains(out, "does not exist") {
		return ""
	}
	return out
}

// replyRule is the rule of the reply option as `ip rule show` prints it.
const replyRule = replyPriority + ":\tfrom all fwmark " + replyMark + " lookup " + replyTable

// replyConflict is routeConflict for table 234 of the reply option: a rule to the table or for the mark that
// is not exactly the installer's, or a route in the table other than the installer's default route (ours: the
// record says the installer added it).
func replyConflict(rules, routes string, ours bool) string {
	for _, l := range strings.Split(rules, "\n") {
		l = strings.TrimSpace(l)
		if (strings.Contains(l, "lookup "+replyTable) || strings.Contains(l, "fwmark "+replyMark)) && l != replyRule {
			return l
		}
	}
	for _, l := range strings.Split(routes, "\n") {
		if l = strings.TrimSpace(l); l != "" && !(ours && strings.HasPrefix(l, "default via ")) {
			return l
		}
	}
	return ""
}

func (ins *Installer) checkRouteTable(cfg Config) error {
	rules, _ := quietCmd("ip", "rule", "show")
	if c := routeConflict(rules, tableRoutes(quietCmd("ip", "route", "show", "table", routeTable))); c != "" {
		return fmt.Errorf("策略路由表 %s 已经被其他配置使用（%s），安装器不会动它", routeTable, c)
	}
	if cfg.Topology == "bypass" && cfg.ReplyVia {
		routes := tableRoutes(quietCmd("ip", "route", "show", "table", replyTable))
		if c := replyConflict(rules, routes, loadChanges(ins.Paths).Reply); c != "" {
			return fmt.Errorf("策略路由表 %s 已经被其他配置使用（%s），安装器不会动它", replyTable, c)
		}
	}
	return nil
}

func (ins *Installer) stepNetwork(cfg Config, j *Job) error {
	st := &InstallState{Version: ins.Version, InstalledAt: time.Now(), UpdatedAt: time.Now(), Topology: cfg.Topology, LAN: cfg.LAN, WAN: cfg.WAN,
		PanelPort: cfg.PanelPort, NftBin: lookPathOr("nft", "/usr/sbin/nft"), SysctlBin: lookPathOr("sysctl", "/usr/sbin/sysctl")}
	if cfg.Topology == "gateway" {
		routes, errs := clientRoutes(cfg.LAN, cfg.LANGateway, cfg.ClientNets, cfg.FakeIPRange, listInterfaces())
		if len(errs) > 0 {
			return fmt.Errorf("客户端网段: %s", errs[0].Msg)
		}
		st.ClientRoutes = routes
	}
	if cfg.Topology == "bypass" && cfg.ReplyVia {
		st.ReplyVia = strings.TrimSpace(cfg.Router)
	}
	ch := loadChanges(ins.Paths)
	if st.ReplyVia != "" {
		if ch.RedirectsBefore == nil {
			ch.RedirectsBefore = map[string]string{}
		}
		use := map[string]bool{}
		for _, dev := range []string{"all", st.LAN[0]} {
			f := "/proc/sys/net/ipv4/conf/" + dev + "/accept_redirects"
			use[f] = true
			if _, ok := ch.RedirectsBefore[f]; ok {
				continue
			}
			if v, err := os.ReadFile(f); err == nil {
				ch.RedirectsBefore[f] = strings.TrimSpace(string(v))
			}
		}
		for f, v := range ch.RedirectsBefore { // the LAN interface changed: the old one gets its value back
			if !use[f] {
				if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
					j.Logf("  （忽略）恢复 %s: %v", f, err)
				}
				delete(ch.RedirectsBefore, f)
			}
		}
		ch.Reply = true
	} else if ch.Reply {
		undoReply(ch, j) // an earlier installation had it: take its rule, table and sysctl back
	}
	owned, err := ownRoutes(ch.Routes, st.ClientRoutes, func(r ClientRoute) string {
		out, _ := quietCmd("ip", "-4", "route", "show", "exact", r.Net)
		return out
	}, j)
	if err != nil {
		return err
	}
	ch.Routes = owned
	ch.Network = true // from here on the installer owns the network changes, until they are taken back
	if cfg.Topology == "gateway" && ch.IPForwardBefore == "" {
		if v, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil {
			ch.IPForwardBefore = strings.TrimSpace(string(v))
		}
	}
	if err := saveChanges(ins.Paths, ch); err != nil {
		return err
	}
	if err := writeJSON(ins.Paths.State(), st); err != nil {
		return err
	}
	return ins.writeNetworkFiles(st, j)
}

func (ins *Installer) stepStart(cfg Config, j *Job) error {
	init := ins.Paths.Init()
	if err := init.Enable(j, singBoxUnitName, dnsUnitName); err != nil {
		return err
	}
	// A configuration left by an earlier installation is in sync with the settings atomdns finds, so atomdns
	// would not start sing-box: start it here. Without one, atomdns applies the first configuration and starts it.
	if _, err := os.Stat(filepath.Join(ins.Paths.Data(), "sing-box", "config.json")); err == nil {
		if err := init.Restart(j, singBoxUnitName); err != nil {
			ins.journal(j, singBoxUnitName)
			return err
		}
	}
	if err := init.Restart(j, dnsUnitName); err != nil {
		ins.journal(j, dnsUnitName)
		return err
	}
	api := newAPIClient(cfg.PanelPort, ins.Paths.Data())
	if _, err := api.waitStatus(60*time.Second, func(*apiStatus) bool { return true }); err != nil {
		ins.journal(j, dnsUnitName)
		return fmt.Errorf("atomdns 的管理接口没有起来: %w", err)
	}
	j.Logf("atomdns 已启动")
	return nil
}

func (ins *Installer) journal(j *Job, unit string) {
	for _, l := range strings.Split(ins.Paths.Init().Logs(unit), "\n") {
		j.Logf("  %s", l)
	}
}

func (ins *Installer) stepSettings(cfg Config, j *Job) error {
	api := newAPIClient(cfg.PanelPort, ins.Paths.Data())
	// the first sing-box configuration is applied by atomdns at start; wait for it before changing more
	if _, err := api.waitStatus(90*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync }); err != nil {
		return fmt.Errorf("sing-box 没有起来: %w", err)
	}
	j.Logf("sing-box 已启动")
	if cfg.keepData {
		j.Logf("沿用已有的设置、规则和节点")
		setLogin(api, cfg, j)
		return nil
	}
	defer setLogin(api, cfg, j)
	warn := func(what string, err error) { j.warn("%s 没有应用（可以在面板里改）：%v", what, err) }
	for _, g := range []struct {
		tag, what, text string
		foreign         bool
		prefix          string
	}{{"domestic", "国内上游", cfg.Domestic, false, "d"}, {"foreign", "国外上游", cfg.Foreign, true, "f"}} {
		want, _ := parseUpstreams(g.text, g.foreign, g.prefix)
		same, err := api.sameUpstreams(g.tag, want)
		switch {
		case err != nil:
			warn(g.what, err)
		case same:
		default:
			if err := api.call("PUT", "/upstreams/"+g.tag, map[string]any{"upstreams": want}, nil); err != nil {
				warn(g.what, err)
			} else {
				j.Logf("已应用 %s", g.what)
			}
		}
	}
	var cur struct {
		FakeIPRange string `json:"fakeip_range"`
		MixedPort   int    `json:"mixed_port"`
	}
	if err := api.call("GET", "/settings", nil, &cur); err != nil {
		warn("sing-box 设置", err)
	} else {
		change := map[string]any{}
		if r := strings.TrimSpace(cfg.FakeIPRange); r != cur.FakeIPRange {
			change["fakeip_range"] = r
		}
		if cfg.MixedPort != cur.MixedPort {
			change["mixed_port"] = cfg.MixedPort
		}
		if len(change) > 0 {
			if err := api.call("PUT", "/settings", change, nil); err != nil {
				warn("sing-box 设置", err)
			} else {
				j.Logf("已应用 sing-box 设置")
			}
		}
	}
	if strings.TrimSpace(cfg.Nodes) != "" {
		ids, err := importNodes(api, cfg.Nodes)
		if err != nil {
			warn("节点", err)
		} else {
			j.Logf("已导入 %d 个节点，放进「默认代理」组", len(ids))
		}
	}
	return nil
}

// setLogin sets the panel login the wizard was given (last: from then on every request needs the key).
func setLogin(api *apiClient, cfg Config, j *Job) {
	user := strings.TrimSpace(cfg.PanelUser)
	if user == "" {
		return
	}
	if err := api.call("PUT", "/auth", map[string]any{"username": user, "password": cfg.PanelPassword}, nil); err != nil {
		j.warn("面板登录没有设置（可以在面板的设置页里设）：%v", err)
		return
	}
	j.Logf("已设置面板登录，用户名 %s", user)
}

// importNodes imports the pasted nodes and makes them the members of the built-in default group; a node that
// is in no group carries no traffic, and the default group is what Final selects.
func importNodes(api *apiClient, text string) ([]string, error) {
	var out struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := api.call("POST", "/nodes", map[string]any{"format": nodeFormat(text), "content": text}, &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out.Nodes))
	for i, n := range out.Nodes {
		ids[i] = n.ID
	}
	var g struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := api.call("GET", "/groups/"+defaultGroup, nil, &g); err != nil {
		return ids, fmt.Errorf("节点已导入，但读不到默认组：%w", err)
	}
	body := map[string]any{"name": g.Name, "type": g.Type, "members": ids}
	if err := api.call("PUT", "/groups/"+defaultGroup, body, nil); err != nil {
		return ids, fmt.Errorf("节点已导入，但没能放进默认组：%w", err)
	}
	return ids, nil
}

func (ins *Installer) stepCheck(cfg Config, j *Job) error {
	api := newAPIClient(cfg.PanelPort, ins.Paths.Data())
	st, err := api.waitStatus(90*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync })
	if err != nil {
		msg := "sing-box 没有处于最新配置"
		if st != nil && st.LastApply != nil && st.LastApply.Error != "" {
			msg += "：" + st.LastApply.Error
		}
		return fmt.Errorf("%s", msg)
	}
	for _, u := range []string{dnsUnitName, singBoxUnitName} {
		if out := ins.Paths.Init().IsActive(u); out != "active" {
			return fmt.Errorf("%s 不是 active（%s）", u, out)
		}
	}
	j.Logf("两个服务都在运行，atomdns %s", st.AtomDNS.Version)
	var cur struct {
		FakeIPRange string `json:"fakeip_range"`
		MixedPort   int    `json:"mixed_port"`
	}
	if err := api.call("GET", "/settings", nil, &cur); err != nil {
		return err
	}
	ls, err := listeners()
	if err == nil {
		for _, want := range []struct {
			port int
			what string
		}{{53, "DNS"}, {cfg.PanelPort, "面板"}, {cur.MixedPort, "mixed 代理"}, {7892, "redirect"}, {7893, "TProxy"}} {
			found := false
			for _, l := range ls {
				if l.Port == want.port {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("没有程序在监听 %s 端口 %d", want.what, want.port)
			}
		}
		j.Logf("端口都在监听")
	}
	_, fake, ferr := net.ParseCIDR(cur.FakeIPRange)
	if ferr != nil {
		return ferr
	}
	if ips, err := lookupVia("127.0.0.1:53", "www.google.com"); err != nil {
		j.warn("DNS 自检（国外域名）没有应答：%v", err)
	} else if len(ips) > 0 && fake.Contains(ips[0]) {
		j.Logf("DNS 自检：www.google.com → %s（FakeIP）", ips[0])
	} else {
		j.warn("DNS 自检：www.google.com → %v，不在 FakeIP 网段 %s 内", ips, fake)
	}
	if ips, err := lookupVia("127.0.0.1:53", "www.baidu.com"); err != nil {
		j.warn("DNS 自检（国内域名）没有应答：%v", err)
	} else if len(ips) > 0 && !fake.Contains(ips[0]) {
		j.Logf("DNS 自检：www.baidu.com → %s（真实 IP）", ips[0])
	} else {
		j.warn("DNS 自检：www.baidu.com → %v，不该是 FakeIP", ips)
	}
	return nil
}

func lookupVia(server, name string) ([]net.IP, error) {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", server)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return r.LookupIP(ctx, "ip4", name)
}

// rollbackNetwork takes the live network changes back after a failed installation (files stay, so a retry or
// the uninstaller finds them). It does nothing when the installer has not changed the network.
func (ins *Installer) rollbackNetwork(log Logger) { teardownNetwork(ins.Paths, log) }

// teardownNetwork stops the services and removes nft tables, policy routing and ip_forward changes, when the
// record says they are the installer's, and then clears that record.
func teardownNetwork(p Paths, log Logger) {
	ch := loadChanges(p)
	if !ch.Network {
		log.Logf("网络部分不是安装器装的（或已经撤销），不动")
		return
	}
	log.Logf("撤销网络部分")
	ignore := func(name string, args ...string) {
		if err := runCmd(log, 60*time.Second, name, args...); err != nil {
			log.Logf("  （忽略）%v", err)
		}
	}
	if err := p.Init().DisableNow(log, dnsUnitName, singBoxUnitName); err != nil {
		log.Logf("  （忽略）%v", err)
	}
	ignore("nft", "delete", "table", "inet", "atom")
	ignore("nft", "delete", "table", "ip", "atom_nat")
	ignore("ip", "rule", "del", "fwmark", fwmark, "lookup", routeTable, "priority", routePriority)
	ignore("ip", "route", "flush", "table", routeTable)
	if ch.Reply {
		undoReply(ch, log)
	}
	for _, r := range ch.Routes {
		ignore("ip", "route", "del", r.Net, "via", r.Via, "dev", r.Dev)
	}
	if ch.IPForwardBefore != "" {
		if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(ch.IPForwardBefore), 0o644); err != nil {
			log.Logf("  （忽略）恢复 ip_forward: %v", err)
		}
	}
	ch.Network, ch.IPForwardBefore, ch.Routes = false, "", nil
	if err := saveChanges(p, ch); err != nil {
		log.Logf("记录保存失败：%v", err)
	}
}

// undoReply removes the rule and table of the reply option and puts accept_redirects back.
func undoReply(ch *Changes, log Logger) {
	for _, args := range [][]string{{"rule", "del", "fwmark", replyMark, "lookup", replyTable, "priority", replyPriority}, {"route", "del", "default", "table", replyTable}} {
		if err := runCmd(log, 30*time.Second, "ip", args...); err != nil {
			log.Logf("  （忽略）%v", err)
		}
	}
	for f, v := range ch.RedirectsBefore {
		if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
			log.Logf("  （忽略）恢复 %s: %v", f, err)
		}
	}
	ch.Reply, ch.RedirectsBefore = false, nil
}

// ownRoutes decides which client routes the installer owns: those it added before and still wants, and new
// ones that are not in the routing table yet. A route the machine already has with the same gateway and
// interface stays the machine's; one with another gateway is a conflict. Routes it added before and no longer
// wants are deleted here. existing returns `ip -4 route show exact <net>`.
func ownRoutes(before, want []ClientRoute, existing func(ClientRoute) string, log Logger) ([]ClientRoute, error) {
	had := map[ClientRoute]bool{}
	for _, r := range before {
		had[r] = true
	}
	keep := map[ClientRoute]bool{}
	var owned []ClientRoute
	for _, r := range want {
		keep[r] = true
		if had[r] {
			owned = append(owned, r)
			continue
		}
		out := strings.TrimSpace(existing(r))
		if out == "" {
			owned = append(owned, r)
			continue
		}
		if strings.Contains(out, "via "+r.Via+" ") && strings.Contains(out+" ", "dev "+r.Dev+" ") {
			log.Logf("路由 %s via %s 已经有了，保留原样", r.Net, r.Via)
			continue
		}
		return nil, fmt.Errorf("网段 %s 已经有路由（%s），和填写的网关 %s 不一致", r.Net, out, r.Via)
	}
	for _, r := range before {
		if !keep[r] {
			if err := runCmd(log, 30*time.Second, "ip", "route", "del", r.Net, "via", r.Via, "dev", r.Dev); err != nil {
				log.Logf("  （忽略）%v", err)
			}
		}
	}
	return owned, nil
}
