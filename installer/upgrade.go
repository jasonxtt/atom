package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
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

	oldVersion := st.Version
	err := ins.applyUpgrade(st, oldSingBox, log)
	if err == nil {
		st.Version, st.UpdatedAt = ins.Version, time.Now()
		if err = writeJSON(p.State(), st); err != nil {
			err = fmt.Errorf("记录新版本失败: %w", err)
		}
	}
	if err != nil {
		st.Version = oldVersion
		log.Logf("升级失败：%v", err)
		log.Logf("恢复备份")
		if rerr := ins.restoreBackup(backup, st.PanelPort, log); rerr != nil {
			return fmt.Errorf("%w（恢复备份也失败了：%v，备份在 %s）", err, rerr, backup)
		}
		return fmt.Errorf("%w（已恢复到 %s）", err, st.Version)
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

// backupItem is something the upgrade may replace. exact items are put back as they were (a file the new
// version added is removed); the others are laid over.
type backupItem struct {
	path, name string
	file       bool
	exact      bool
}

// backed up: programs, the state and network files, the shipped configuration (not the data), the WebUI.
func (ins *Installer) backupItems() []backupItem {
	p := ins.Paths
	return []backupItem{
		{path: filepath.Join(p.Root, "bin"), name: "bin"},
		{path: filepath.Join(p.Root, "etc"), name: "etc"},
		{path: filepath.Join(p.Root, "lib"), name: "lib"},
		{path: filepath.Join(p.Data(), "sub_config"), name: "data/sub_config", exact: true},
		{path: filepath.Join(p.Data(), "ui", "main"), name: "data/ui/main", exact: true},
		{path: filepath.Join(p.Data(), "config_custom.yaml"), name: "data/config_custom.yaml", file: true},
		{path: filepath.Join(p.Data(), "rule", "atom_catalog.json"), name: "data/rule/atom_catalog.json", file: true},
		{path: p.DNSUnit(), name: "units/" + dnsUnitName, file: true},
		{path: p.SingBoxUnit(), name: "units/" + singBoxUnitName, file: true},
	}
}

const absentList = "absent.txt"

func (ins *Installer) makeBackup(dir string) error {
	var absent []string
	defer func() {
		if len(absent) > 0 {
			os.WriteFile(filepath.Join(dir, absentList), []byte(strings.Join(absent, "\n")+"\n"), 0o644)
		}
	}()
	for _, it := range ins.backupItems() {
		if _, err := os.Stat(it.path); err != nil {
			absent = append(absent, it.name)
			continue
		}
		var err error
		if it.file {
			err = copyFileAtomic(it.path, filepath.Join(dir, it.name), 0o644)
		} else {
			err = copyTree(it.path, filepath.Join(dir, it.name))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (ins *Installer) restoreBackup(dir string, panelPort int, log Logger) error {
	absent := map[string]bool{}
	if data, err := os.ReadFile(filepath.Join(dir, absentList)); err == nil {
		for _, n := range strings.Fields(string(data)) {
			absent[n] = true
		}
	}
	for _, it := range ins.backupItems() {
		src := filepath.Join(dir, it.name)
		if _, err := os.Stat(src); err != nil {
			if absent[it.name] && (it.file || it.exact) {
				os.RemoveAll(it.path) // the failed upgrade created it
			}
			continue
		}
		var err error
		if it.file {
			err = copyFileAtomic(src, it.path, 0o644)
		} else {
			if it.exact {
				os.RemoveAll(it.path)
			}
			err = copyTree(src, it.path)
		}
		if err != nil {
			return err
		}
	}
	if err := runCmd(log, 30*time.Second, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCmd(log, 60*time.Second, "systemctl", "restart", dnsUnitName); err != nil {
		return err
	}
	if err := runCmd(log, 60*time.Second, "systemctl", "restart", singBoxUnitName); err != nil {
		return err
	}
	if _, err := newAPIClient(panelPort).waitStatus(60*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync }); err != nil {
		return fmt.Errorf("恢复后服务没有起来: %w", err)
	}
	log.Logf("已恢复，服务正常")
	return nil
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
