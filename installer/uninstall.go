package main

import (
	"os"
	"path/filepath"
	"time"
)

// uninstall takes atom off the machine: services, nft tables, policy routing, ip_forward, what the wizard
// changed (resolved, stopped services), the units and /opt/atom. The data directory stays unless purge.
func uninstall(p Paths, purge bool, log Logger) error {
	teardownNetwork(p, log)
	for _, f := range []string{p.DNSUnit(), p.SingBoxUnit()} {
		os.Remove(f)
	}
	_ = runCmd(log, 30*time.Second, "systemctl", "daemon-reload")
	_ = runCmd(log, 30*time.Second, "systemctl", "reset-failed", dnsUnitName, singBoxUnitName)

	ch := loadChanges(p)
	var firstErr error
	if err := restoreResolved(p, ch, log); err != nil {
		log.Logf("还原 systemd-resolved 失败：%v", err)
		firstErr = err
	}
	for _, u := range ch.DisabledUnits {
		if err := runCmd(log, 60*time.Second, "systemctl", "enable", "--now", u); err != nil {
			log.Logf("重新启用 %s 失败：%v", u, err)
		}
	}
	if len(ch.AptInstalled) > 0 {
		log.Logf("安装时装过的软件包没有卸载：%v", ch.AptInstalled)
	}
	if purge {
		log.Logf("删除 %s（含数据）", p.Root)
		return firstErr2(os.RemoveAll(p.Root), firstErr)
	}
	for _, d := range []string{"bin", "etc", "lib", "run", "pkg", "backup"} {
		os.RemoveAll(filepath.Join(p.Root, d))
	}
	log.Logf("数据保留在 %s", p.Data())
	return firstErr
}

func firstErr2(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
