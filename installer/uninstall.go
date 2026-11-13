package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hasRecords: install.json or changes.json is there, so there is something to take back.
func hasRecords(p Paths) bool {
	for _, f := range []string{p.State(), p.Changes()} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// uninstall takes atom off the machine: services, nft tables, policy routing, ip_forward, what the wizard
// changed (resolved, stopped services), the units and /opt/atom. The data directory stays unless purge.
// When something the installer changed cannot be put back, the records and the program stay so that the
// command can be run again once the cause is fixed.
func uninstall(p Paths, purge bool, log Logger) error {
	teardownNetwork(p, log)
	if _, err := os.Stat(p.State()); err == nil { // the units are written after install.json
		for _, f := range []string{p.DNSUnit(), p.SingBoxUnit()} {
			os.Remove(f)
		}
		p.Init().Forget(log, dnsUnitName, singBoxUnitName)
	}

	ch := loadChanges(p)
	var failed []string
	if err := restoreResolved(p, ch, log); err != nil {
		log.Logf("还原 systemd-resolved 失败：%v", err)
		failed = append(failed, "systemd-resolved")
	}
	if err := restoreResolvConf(p, ch, log); err != nil {
		log.Logf("还原 /etc/resolv.conf 失败：%v", err)
		failed = append(failed, "/etc/resolv.conf")
	}
	var left []UnitState
	for _, u := range ch.DisabledUnits {
		if err := restoreUnit(p, u, log); err != nil {
			log.Logf("还原 %s 失败：%v", u.Unit, err)
			left = append(left, u)
			failed = append(failed, u.Unit)
		}
	}
	ch.DisabledUnits = left
	if len(failed) > 0 {
		if err := saveChanges(p, ch); err != nil {
			log.Logf("记录保存失败：%v", err)
		}
		return fmt.Errorf("没有还原：%s。记录和程序都还在，处理好之后可以再运行一次 uninstall", strings.Join(failed, "、"))
	}
	if p.OpenRC {
		os.RemoveAll(openRCLogDir) // logs are not data: gone with or without purge
	}
	if len(ch.PkgInstalled) > 0 {
		log.Logf("安装时装过的软件包没有卸载：%v", ch.PkgInstalled)
	}
	if purge {
		log.Logf("删除 %s（含数据）", p.Root)
		return os.RemoveAll(p.Root)
	}
	for _, d := range []string{"bin", "etc", "lib", "run", "pkg", "backup"} {
		os.RemoveAll(filepath.Join(p.Root, d))
	}
	log.Logf("数据保留在 %s", p.Data())
	return nil
}
