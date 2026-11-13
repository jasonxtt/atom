package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	resolvedDropIn     = "/etc/systemd/resolved.conf.d/atom.conf"
	resolvedMark       = "# written by atom-installer"
	resolvedUpstreams  = "/run/systemd/resolve/resolv.conf"
	resolvedStubTarget = "stub-resolv.conf"
)

// freeResolvedStub turns off the DNS stub listener of systemd-resolved (127.0.0.53 and 127.0.0.54 port 53)
// and points /etc/resolv.conf at the real upstream servers, so this machine can still resolve names. What will
// be changed is recorded before it is changed; a failure part way puts things back.
func freeResolvedStub(p Paths, log Logger) error {
	if data, err := os.ReadFile(resolvedDropIn); err == nil && !strings.HasPrefix(string(data), resolvedMark) {
		return fmt.Errorf("%s 已经存在，不是安装器写的；请手动处理 systemd-resolved", resolvedDropIn)
	}
	ch := loadChanges(p)
	if target, err := os.Readlink("/etc/resolv.conf"); err == nil && strings.Contains(target, resolvedStubTarget) && ch.ResolvConfLink == "" {
		ch.ResolvConfLink = target
	}
	ch.ResolvedDropIn = true
	if err := saveChanges(p, ch); err != nil {
		return err
	}
	err := func() error {
		if err := os.MkdirAll(filepath.Dir(resolvedDropIn), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(resolvedDropIn, []byte(resolvedMark+"\n[Resolve]\nDNSStubListener=no\n"), 0o644); err != nil {
			return err
		}
		if ch.ResolvConfLink != "" {
			if err := replaceSymlink("/etc/resolv.conf", resolvedUpstreams); err != nil {
				return err
			}
		}
		return runCmd(log, 30*time.Second, "systemctl", "restart", "systemd-resolved.service")
	}()
	if err != nil {
		log.Logf("没有做成，撤回：%v", err)
		if rerr := restoreResolved(p, ch, log); rerr != nil {
			log.Logf("撤回时也出错：%v", rerr)
		}
		_ = saveChanges(p, ch)
	}
	return err
}

func replaceSymlink(link, target string) error {
	tmp := link + ".atom-new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// restoreResolved undoes freeResolvedStub. The record in ch is cleared only for what was put back.
func restoreResolved(p Paths, ch *Changes, log Logger) error {
	if !ch.ResolvedDropIn {
		return nil
	}
	if data, err := os.ReadFile(resolvedDropIn); err == nil && strings.HasPrefix(string(data), resolvedMark) {
		if err := os.Remove(resolvedDropIn); err != nil && !os.IsNotExist(err) {
			return err
		}
		os.Remove(filepath.Dir(resolvedDropIn)) // only if empty
	}
	if ch.ResolvConfLink != "" {
		if err := replaceSymlink("/etc/resolv.conf", ch.ResolvConfLink); err != nil {
			return err
		}
		ch.ResolvConfLink = ""
	}
	if err := runCmd(log, 30*time.Second, "systemctl", "restart", "systemd-resolved.service"); err != nil {
		return err
	}
	ch.ResolvedDropIn = false
	return nil
}

// freeUnit stops and disables a systemd service that holds a port; how it was is recorded first.
func freeUnit(p Paths, unit string, log Logger) error {
	if isOurUnit(unit) || !strings.HasSuffix(unit, ".service") || strings.ContainsAny(unit, " /") {
		return fmt.Errorf("不能停用 %q", unit)
	}
	sd := systemdInit{}
	ch := loadChanges(p)
	known := false
	for _, u := range ch.DisabledUnits {
		known = known || u.Unit == unit
	}
	enabled := sd.IsEnabled(unit)
	if !known {
		ch.DisabledUnits = append(ch.DisabledUnits, UnitState{Unit: unit, WasEnabled: enabled, WasActive: sd.IsActive(unit) == "active"})
		if err := saveChanges(p, ch); err != nil {
			return err
		}
	}
	if enabled {
		return sd.DisableNow(log, unit)
	}
	return sd.Stop(log, unit)
}

// restoreUnit puts a service back as it was: enabled or not, started or not (OpenRC: in each runlevel it was in).
func restoreUnit(p Paths, u UnitState, log Logger) error {
	if u.OpenRC {
		for _, l := range u.Runlevels {
			if err := runCmd(log, 30*time.Second, "rc-update", "add", u.Unit, l); err != nil {
				return err
			}
		}
		if u.WasActive {
			return openRCInit{}.Start(log, u.Unit)
		}
		return nil
	}
	sd := systemdInit{}
	if u.WasEnabled {
		if err := sd.Enable(log, u.Unit); err != nil {
			return err
		}
	}
	if u.WasActive {
		return sd.Start(log, u.Unit)
	}
	return nil
}

var (
	resolvConfPath = "/etc/resolv.conf"
	runlevelsDir   = "/etc/runlevels"
)

// loopbackOnly: resolv.conf names at least one server and all of them are on this machine, so names stop
// resolving once the service that holds port 53 is stopped.
func loopbackOnly(text string) bool {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(strings.SplitN(f[1], "%", 2)[0])
		if ip == nil || !ip.IsLoopback() {
			return false
		}
		n++
	}
	return n > 0
}

// runlevelsOf is the runlevels (directories of dir) that hold the OpenRC service svc.
func runlevelsOf(dir, svc string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if _, err := os.Lstat(filepath.Join(dir, e.Name(), svc)); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// freeOpenRCService stops an OpenRC service that holds a port and takes it out of its runlevels; how it was is
// recorded first. When resolv.conf only names this machine, the dependencies are installed first (while names
// still resolve) and resolv.conf then points at the default gateway; its content is saved for the uninstall.
func freeOpenRCService(p Paths, svc string, log Logger) error {
	if isOurUnit(svc) || svc == "" || strings.ContainsAny(svc, " /") {
		return fmt.Errorf("不能停用 %q", svc)
	}
	conf, _ := os.ReadFile(resolvConfPath)
	lo := loopbackOnly(string(conf))
	var gw string
	if lo {
		out, _ := quietCmd("ip", "-4", "route", "show", "default")
		var gws []string
		for _, v := range parseDefaultVia(out) {
			gws = append(gws, v)
		}
		sort.Strings(gws)
		if len(gws) == 0 {
			return fmt.Errorf("/etc/resolv.conf 只指向本机，但找不到默认网关可以改成")
		}
		gw = gws[0]
		if err := ensureDeps(p, false, log); err != nil {
			return err
		}
	}
	ch := loadChanges(p)
	known := false
	for _, u := range ch.DisabledUnits {
		known = known || u.Unit == svc
	}
	init := openRCInit{}
	levels := runlevelsOf(runlevelsDir, svc)
	if !known {
		ch.DisabledUnits = append(ch.DisabledUnits, UnitState{Unit: svc, OpenRC: true, Runlevels: levels, WasEnabled: len(levels) > 0, WasActive: init.IsActive(svc) == "active"})
	}
	if lo && !ch.ResolvConfSaved {
		if err := writeFileAtomic(p.Etc("resolv.conf.orig"), conf, 0o644); err != nil {
			return err
		}
		ch.ResolvConfSaved = true
	}
	if err := saveChanges(p, ch); err != nil {
		return err
	}
	if err := init.Stop(log, svc); err != nil {
		return err
	}
	for _, l := range levels {
		if err := runCmd(log, 30*time.Second, "rc-update", "del", svc, l); err != nil {
			return err
		}
	}
	if lo {
		return writeFileAtomic(resolvConfPath, []byte("nameserver "+gw+"\n"), 0o644)
	}
	return nil
}

// restoreResolvConf puts back the resolv.conf that freeOpenRCService replaced.
func restoreResolvConf(p Paths, ch *Changes, log Logger) error {
	if !ch.ResolvConfSaved {
		return nil
	}
	data, err := os.ReadFile(p.Etc("resolv.conf.orig"))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(resolvConfPath, data, 0o644); err != nil {
		return err
	}
	os.Remove(p.Etc("resolv.conf.orig"))
	ch.ResolvConfSaved = false
	log.Logf("已还原 /etc/resolv.conf")
	return nil
}
