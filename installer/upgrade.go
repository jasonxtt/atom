package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// compareVersions compares dotted numeric versions ("0.1.0"); anything after a non-digit is ignored.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	for i, s := range strings.SplitN(v, ".", 3) {
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// upgrade replaces the programs and the shipped configuration, keeps the data, and goes back to the
// backup when the new version does not come up.
func (ins *Installer) upgrade(st *InstallState, log Logger) error {
	p := ins.Paths
	backup := filepath.Join(p.Backup(), fmt.Sprintf("%s-%s", st.Version, time.Now().Format("20060102-150405")))
	log.Logf("备份到 %s", backup)
	if err := ins.makeBackup(backup); err != nil {
		return fmt.Errorf("备份: %w", err)
	}
	oldSingBox := fileSHA256(p.Bin("sing-box"))

	err := ins.applyUpgrade(st, oldSingBox, log)
	if err != nil {
		log.Logf("升级失败：%v", err)
		log.Logf("恢复备份")
		if rerr := ins.restoreBackup(backup, log); rerr != nil {
			return fmt.Errorf("%w（恢复备份也失败了：%v，备份在 %s）", err, rerr, backup)
		}
		return fmt.Errorf("%w（已恢复到 %s）", err, st.Version)
	}
	st.Version, st.UpdatedAt = ins.Version, time.Now()
	if err := writeJSON(p.State(), st); err != nil {
		return err
	}
	pruneBackups(p.Backup(), 3)
	return nil
}

func (ins *Installer) applyUpgrade(st *InstallState, oldSingBox string, log Logger) error {
	if err := ins.installBinaries(log); err != nil {
		return err
	}
	if err := ins.installData(st.PanelPort, log); err != nil {
		return err
	}
	if err := ins.writeNetworkFiles(st, log); err != nil {
		return err
	}
	if err := runCmd(log, 60*time.Second, "systemctl", "restart", dnsUnitName); err != nil {
		return err
	}
	api := newAPIClient(st.PanelPort)
	if _, err := api.waitStatus(60*time.Second, func(*apiStatus) bool { return true }); err != nil {
		return fmt.Errorf("atomdns 的管理接口没有起来: %w", err)
	}
	if fileSHA256(ins.Paths.Bin("sing-box")) != oldSingBox {
		log.Logf("sing-box 程序变了，重启它")
		if err := runCmd(log, 60*time.Second, "systemctl", "restart", singBoxUnitName); err != nil {
			return err
		}
	}
	if _, err := api.waitStatus(90*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync }); err != nil {
		return fmt.Errorf("升级后 sing-box 没有处于最新配置: %w", err)
	}
	return nil
}

// backed up: programs, the state and network files, the units, the shipped configuration (not the data).
func (ins *Installer) backupItems() [][2]string {
	p := ins.Paths
	return [][2]string{{filepath.Join(p.Root, "bin"), "bin"}, {filepath.Join(p.Root, "etc"), "etc"}, {filepath.Join(p.Root, "lib"), "lib"},
		{filepath.Join(p.Data(), "sub_config"), "data/sub_config"}, {filepath.Join(p.Data(), "ui", "main"), "data/ui/main"}}
}

func (ins *Installer) makeBackup(dir string) error {
	p := ins.Paths
	for _, it := range ins.backupItems() {
		if _, err := os.Stat(it[0]); err != nil {
			continue
		}
		if err := copyTree(it[0], filepath.Join(dir, it[1])); err != nil {
			return err
		}
	}
	for _, f := range []string{p.DNSUnit(), p.SingBoxUnit(), filepath.Join(p.Data(), "config_custom.yaml")} {
		rel := "units/" + filepath.Base(f)
		if strings.HasSuffix(f, "config_custom.yaml") {
			rel = "data/config_custom.yaml"
		}
		if err := copyFileAtomic(f, filepath.Join(dir, rel), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (ins *Installer) restoreBackup(dir string, log Logger) error {
	p := ins.Paths
	for _, it := range ins.backupItems() {
		src := filepath.Join(dir, it[1])
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if it[1] == "data/ui/main" {
			os.RemoveAll(it[0])
		}
		if err := copyTree(src, it[0]); err != nil {
			return err
		}
	}
	for _, u := range []string{p.DNSUnit(), p.SingBoxUnit()} {
		if err := copyFileAtomic(filepath.Join(dir, "units", filepath.Base(u)), u, 0o644); err != nil {
			return err
		}
	}
	if err := copyFileAtomic(filepath.Join(dir, "data", "config_custom.yaml"), filepath.Join(p.Data(), "config_custom.yaml"), 0o644); err != nil {
		return err
	}
	if err := runCmd(log, 30*time.Second, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCmd(log, 60*time.Second, "systemctl", "restart", dnsUnitName); err != nil {
		return err
	}
	return runCmd(log, 60*time.Second, "systemctl", "restart", singBoxUnitName)
}

func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		os.RemoveAll(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}
