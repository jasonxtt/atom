package main

import (
	"fmt"
	"os"
	"path/filepath"
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
		os.Remove(resolvedDropIn)
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

func unitProperty(verb, unit string) string {
	out, _ := quietCmd("systemctl", verb, unit)
	return out
}

// freeUnit stops and disables a service that holds a port; how it was is recorded first.
func freeUnit(p Paths, unit string, log Logger) error {
	if isOurUnit(unit) || !strings.HasSuffix(unit, ".service") || strings.ContainsAny(unit, " /") {
		return fmt.Errorf("不能停用 %q", unit)
	}
	ch := loadChanges(p)
	known := false
	for _, u := range ch.DisabledUnits {
		known = known || u.Unit == unit
	}
	enabled := strings.HasPrefix(unitProperty("is-enabled", unit), "enabled")
	if !known {
		ch.DisabledUnits = append(ch.DisabledUnits, UnitState{Unit: unit, WasEnabled: enabled, WasActive: unitProperty("is-active", unit) == "active"})
		if err := saveChanges(p, ch); err != nil {
			return err
		}
	}
	if enabled {
		return runCmd(log, 60*time.Second, "systemctl", "disable", "--now", unit)
	}
	return runCmd(log, 60*time.Second, "systemctl", "stop", unit)
}

// restoreUnit puts a service back as it was: enabled (and started), or only started, or left alone.
func restoreUnit(u UnitState, log Logger) error {
	switch {
	case u.WasEnabled:
		return runCmd(log, 60*time.Second, "systemctl", "enable", "--now", u.Unit)
	case u.WasActive:
		return runCmd(log, 60*time.Second, "systemctl", "start", u.Unit)
	}
	return nil
}
