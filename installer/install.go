package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	return JobView{Steps: append([]stepState(nil), j.steps...), Log: append([]string(nil), j.log[since:]...), Next: len(j.log),
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
	var networkTouched bool
	err := func() error {
		steps := []func() error{
			func() error { return ins.stepDeps(cfg, j) },
			func() error { return ins.stepBinaries(j) },
			func() error { return ins.stepConfig(cfg, j) },
			func() error { networkTouched = true; return ins.stepNetwork(cfg, j) },
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
	if err != nil {
		j.Logf("安装失败：%v", err)
		if networkTouched {
			j.Logf("撤销网络部分")
			ins.rollbackNetwork(j)
		}
		j.finish(err, 0)
		return
	}
	st, err := loadState(ins.Paths)
	if err == nil {
		st.Complete, st.UpdatedAt = true, time.Now()
		err = writeJSON(ins.Paths.State(), st)
	}
	if err != nil {
		j.Logf("记录安装结果失败：%v", err)
		j.finish(err, 0)
		return
	}
	j.finish(nil, cfg.PanelPort)
}

func (ins *Installer) stepDeps(cfg Config, j *Job) error {
	var need []string
	if _, err := exec.LookPath("nft"); err != nil {
		need = append(need, "nftables")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		need = append(need, "iproute2")
	}
	if cfg.Topology == "gateway" {
		if _, err := exec.LookPath("sysctl"); err != nil {
			need = append(need, "procps")
		}
	}
	if len(need) == 0 {
		j.Logf("依赖都在")
		return nil
	}
	if err := runCmd(j, 5*time.Minute, "apt-get", "update"); err != nil {
		return err
	}
	if err := runCmd(j, 10*time.Minute, "apt-get", append([]string{"install", "-y", "--no-install-recommends"}, need...)...); err != nil {
		return err
	}
	ch := loadChanges(ins.Paths)
	ch.AptInstalled = append(ch.AptInstalled, need...)
	return saveChanges(ins.Paths, ch)
}

var installedBinaries = []string{"atomdns", "sing-box", "atom-installer"}

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
	if err := refreshData(filepath.Join(ins.PkgDir, "config"), ins.Paths.Data(), panelPort); err != nil {
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
	nft, err := renderNft(st.LAN)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p.Etc("atom.nft"), []byte(nft), 0o644); err != nil {
		return err
	}
	if st.Topology == "gateway" {
		gw, err := renderGatewayNft(st.LAN, st.WAN)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(p.Etc("atom-gateway.nft"), []byte(gw), 0o644); err != nil {
			return err
		}
	} else {
		os.Remove(p.Etc("atom-gateway.nft"))
	}
	if err := writeFileAtomic(p.Lib("policy-route.sh"), []byte(renderPolicyRoute()), 0o755); err != nil {
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
	if err := writeFileAtomic(p.DNSUnit(), []byte(renderDNSUnit(p, st.NftBin, st.SysctlBin, st.Topology == "gateway")), 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(p.SingBoxUnit(), []byte(renderSingBoxUnit(p)), 0o644); err != nil {
		return err
	}
	return runCmd(log, 30*time.Second, "systemctl", "daemon-reload")
}

func lookPathOr(name, fallback string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return fallback
}

func (ins *Installer) stepNetwork(cfg Config, j *Job) error {
	st := &InstallState{Version: ins.Version, InstalledAt: time.Now(), UpdatedAt: time.Now(), Topology: cfg.Topology, LAN: cfg.LAN, WAN: cfg.WAN,
		PanelPort: cfg.PanelPort, NftBin: lookPathOr("nft", "/usr/sbin/nft"), SysctlBin: lookPathOr("sysctl", "/usr/sbin/sysctl")}
	ch := loadChanges(ins.Paths)
	if cfg.Topology == "gateway" && ch.IPForwardBefore == "" {
		if v, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil {
			ch.IPForwardBefore = strings.TrimSpace(string(v))
			if err := saveChanges(ins.Paths, ch); err != nil {
				return err
			}
		}
	}
	if err := writeJSON(ins.Paths.State(), st); err != nil {
		return err
	}
	return ins.writeNetworkFiles(st, j)
}

func (ins *Installer) stepStart(cfg Config, j *Job) error {
	if err := runCmd(j, 30*time.Second, "systemctl", "enable", singBoxUnitName); err != nil {
		return err
	}
	if err := runCmd(j, 30*time.Second, "systemctl", "enable", dnsUnitName); err != nil {
		return err
	}
	if err := runCmd(j, 60*time.Second, "systemctl", "restart", dnsUnitName); err != nil {
		ins.journal(j, dnsUnitName)
		return err
	}
	api := newAPIClient(cfg.PanelPort)
	if _, err := api.waitStatus(60*time.Second, func(*apiStatus) bool { return true }); err != nil {
		ins.journal(j, dnsUnitName)
		return fmt.Errorf("atomdns 的管理接口没有起来: %w", err)
	}
	j.Logf("atomdns 已启动")
	return nil
}

func (ins *Installer) journal(j *Job, unit string) {
	out, _ := quietCmd("journalctl", "-u", unit, "-n", "15", "--no-pager")
	for _, l := range strings.Split(out, "\n") {
		j.Logf("  %s", l)
	}
}

func (ins *Installer) stepSettings(cfg Config, j *Job) error {
	api := newAPIClient(cfg.PanelPort)
	// the first sing-box configuration is applied by atomdns at start; wait for it before changing more
	if _, err := api.waitStatus(90*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync }); err != nil {
		return fmt.Errorf("sing-box 没有起来: %w", err)
	}
	j.Logf("sing-box 已启动")
	put := func(what, path string, body any) {
		if err := api.call("PUT", path, body, nil); err != nil {
			j.warn("%s 没有应用（可以在面板里改）：%v", what, err)
		} else {
			j.Logf("已应用 %s", what)
		}
	}
	if normalizeLines(cfg.Domestic) != normalizeLines(defaultDomestic) {
		ups, _ := parseUpstreams(cfg.Domestic, false, "d")
		put("国内上游", "/upstreams/domestic", map[string]any{"upstreams": ups})
	}
	if normalizeLines(cfg.Foreign) != normalizeLines(defaultForeign) {
		ups, _ := parseUpstreams(cfg.Foreign, true, "f")
		put("国外上游", "/upstreams/foreign", map[string]any{"upstreams": ups})
	}
	settings := map[string]any{}
	if strings.TrimSpace(cfg.FakeIPRange) != defaultFakeIP {
		settings["fakeip_range"] = strings.TrimSpace(cfg.FakeIPRange)
	}
	if cfg.MixedPort != defaultMixed {
		settings["mixed_port"] = cfg.MixedPort
	}
	if len(settings) > 0 {
		put("sing-box 设置", "/settings", settings)
	}
	if strings.TrimSpace(cfg.Nodes) != "" {
		var out struct {
			Nodes []any `json:"nodes"`
		}
		err := api.call("POST", "/nodes", map[string]any{"format": nodeFormat(cfg.Nodes), "content": cfg.Nodes}, &out)
		if err != nil {
			j.warn("节点没有导入（可以在面板里导入）：%v", err)
		} else {
			j.Logf("已导入 %d 个节点", len(out.Nodes))
		}
	}
	return nil
}

func (ins *Installer) stepCheck(cfg Config, j *Job) error {
	api := newAPIClient(cfg.PanelPort)
	st, err := api.waitStatus(90*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync })
	if err != nil {
		msg := "sing-box 没有处于最新配置"
		if st != nil && st.LastApply != nil && st.LastApply.Error != "" {
			msg += "：" + st.LastApply.Error
		}
		return fmt.Errorf("%s", msg)
	}
	for _, u := range []string{dnsUnitName, singBoxUnitName} {
		if out, _ := quietCmd("systemctl", "is-active", u); out != "active" {
			return fmt.Errorf("%s 不是 active（%s）", u, out)
		}
	}
	j.Logf("两个服务都在运行，atomdns %s", st.AtomDNS.Version)
	ls, err := listeners()
	if err == nil {
		for _, want := range []struct {
			port int
			what string
		}{{53, "DNS"}, {cfg.PanelPort, "面板"}, {cfg.MixedPort, "mixed 代理"}, {7892, "redirect"}, {7893, "TProxy"}} {
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
	_, fake, ferr := net.ParseCIDR(strings.TrimSpace(cfg.FakeIPRange))
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

// rollbackNetwork takes the live network changes back; files stay so a retry (or the uninstaller) finds them.
func (ins *Installer) rollbackNetwork(log Logger) {
	teardownNetwork(ins.Paths, log)
}

// teardownNetwork stops the services and removes nft tables, policy routing and ip_forward changes.
func teardownNetwork(p Paths, log Logger) {
	ignore := func(name string, args ...string) {
		if err := runCmd(log, 60*time.Second, name, args...); err != nil {
			log.Logf("  （忽略）%v", err)
		}
	}
	ignore("systemctl", "disable", "--now", dnsUnitName, singBoxUnitName)
	ignore("nft", "delete", "table", "inet", "atom")
	ignore("nft", "delete", "table", "ip", "atom_nat")
	ignore("ip", "rule", "del", "fwmark", fwmark, "lookup", routeTable, "priority", routePriority)
	ignore("ip", "route", "flush", "table", routeTable)
	if ch := loadChanges(p); ch.IPForwardBefore != "" {
		if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(ch.IPForwardBefore), 0o644); err != nil {
			log.Logf("  （忽略）恢复 ip_forward: %v", err)
		}
	}
}
