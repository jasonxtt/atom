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
	resolvedUpstreams  = "/run/systemd/resolve/resolv.conf"
	resolvedStubTarget = "stub-resolv.conf"
)

// freeResolvedStub turns off the DNS stub listener of systemd-resolved (127.0.0.53 and 127.0.0.54 port 53)
// and points /etc/resolv.conf at the real upstream servers, so this machine can still resolve names.
func freeResolvedStub(p Paths, log Logger) error {
	ch := loadChanges(p)
	if err := os.MkdirAll(filepath.Dir(resolvedDropIn), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(resolvedDropIn, []byte("# written by atom-installer\n[Resolve]\nDNSStubListener=no\n"), 0o644); err != nil {
		return err
	}
	ch.ResolvedDropIn = true
	if target, err := os.Readlink("/etc/resolv.conf"); err == nil && strings.Contains(target, resolvedStubTarget) {
		if ch.ResolvConfLink == "" {
			ch.ResolvConfLink = target
		}
		if err := replaceSymlink("/etc/resolv.conf", resolvedUpstreams); err != nil {
			return err
		}
	}
	if err := saveChanges(p, ch); err != nil {
		return err
	}
	return runCmd(log, 30*time.Second, "systemctl", "restart", "systemd-resolved.service")
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

// restoreResolved undoes freeResolvedStub.
func restoreResolved(p Paths, ch *Changes, log Logger) error {
	if !ch.ResolvedDropIn {
		return nil
	}
	os.Remove(resolvedDropIn)
	os.Remove(filepath.Dir(resolvedDropIn)) // only if empty
	if ch.ResolvConfLink != "" {
		if err := replaceSymlink("/etc/resolv.conf", ch.ResolvConfLink); err != nil {
			return err
		}
	}
	ch.ResolvedDropIn, ch.ResolvConfLink = false, ""
	return runCmd(log, 30*time.Second, "systemctl", "restart", "systemd-resolved.service")
}

// freeUnit stops and disables a service that holds a port.
func freeUnit(p Paths, unit string, log Logger) error {
	if isOurUnit(unit) || !strings.HasSuffix(unit, ".service") || strings.ContainsAny(unit, " /") {
		return fmt.Errorf("不能停用 %q", unit)
	}
	ch := loadChanges(p)
	if err := runCmd(log, 60*time.Second, "systemctl", "disable", "--now", unit); err != nil {
		return err
	}
	for _, u := range ch.DisabledUnits {
		if u == unit {
			return saveChanges(p, ch)
		}
	}
	ch.DisabledUnits = append(ch.DisabledUnits, unit)
	return saveChanges(p, ch)
}
