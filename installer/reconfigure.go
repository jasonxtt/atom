package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// reconfigureRun is a reconfigure under way: the backup taken before the network changes and the records of
// the state it started from.
type reconfigureRun struct {
	dir   string
	oldSt InstallState
	oldCh Changes
}

// beginReconfigure backs up what the run changes (the upgrade's backup: programs, nft rules, service files,
// policy-route.sh, install.json, changes.json, the shipped configuration).
func (ins *Installer) beginReconfigure(log Logger) (*reconfigureRun, error) {
	st, err := loadState(ins.Paths)
	if err != nil || !st.Complete {
		return nil, fmt.Errorf("没有已完成的安装")
	}
	rc := &reconfigureRun{dir: filepath.Join(ins.Paths.Backup(), "reconfigure-"+time.Now().Format("20060102-150405")), oldSt: *st, oldCh: *loadChanges(ins.Paths)}
	if err := ins.makeBackup(rc.dir); err != nil {
		return nil, fmt.Errorf("备份: %w", err)
	}
	log.Logf("备份到 %s", rc.dir)
	return rc, nil
}

// restore puts the old configuration back after a failed reconfigure: whatever the new state added that the old
// one did not have, then the files (the panel login included) and the services (their start step loads the old
// nft rules and routing again). It never takes the network down.
func (rc *reconfigureRun) restore(ins *Installer, log Logger) {
	log.Logf("恢复备份")
	newCh := loadChanges(ins.Paths)
	rc.quiesce(ins.Paths.Init(), log)
	// first what the new state added (ip_forward back to its old value resets accept_redirects of the
	// interfaces, which the old services then turn off again when they start)
	rc.undoAdditions(newCh, log)
	if err := ins.restoreBackup(rc.dir, rc.oldSt.PanelPort, log); err != nil {
		log.Logf("恢复备份出错：%v（备份在 %s）", err, rc.dir)
	}
}

// quiesce stops the new services (and with them any restart the service manager still has pending for them), so
// that nothing recreates what is undone next.
func (rc *reconfigureRun) quiesce(init initSystem, log Logger) {
	for _, n := range []string{dnsUnitName, singBoxUnitName} {
		if err := init.Stop(log, n); err != nil {
			log.Logf("  （忽略）%v", err)
		}
	}
}

// undoAdditions removes what a run left in the live network that the old state does not have: the NAT table
// and ip_forward of a gateway, the reply rule and table 234, client routes the installer added.
func (rc *reconfigureRun) undoAdditions(newCh *Changes, log Logger) {
	if rc.oldSt.Topology != "gateway" && newCh.IPForwardBefore != "" && rc.oldCh.IPForwardBefore == "" {
		undoGateway(newCh, log)
	}
	if rc.oldSt.ReplyVia == "" && newCh.Reply {
		undoReply(newCh, log)
	}
	for f, v := range newCh.RedirectsBefore { // an interface only the new state turned redirects off for
		if _, old := rc.oldCh.RedirectsBefore[f]; !old {
			if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
				log.Logf("  （忽略）恢复 %s: %v", f, err)
			}
		}
	}
	had := map[ClientRoute]bool{}
	for _, r := range rc.oldCh.Routes {
		had[r] = true
	}
	for _, r := range newCh.Routes {
		if !had[r] {
			if err := runCmd(log, 30*time.Second, "ip", "route", "del", r.Net, "via", r.Via, "dev", r.Dev); err != nil {
				log.Logf("  （忽略）%v", err)
			}
		}
	}
}

// configFromState is what the wizard shows for an installed machine.
func configFromState(st *InstallState) Config {
	c := Config{Topology: st.Topology, LAN: st.LAN, WAN: st.WAN, AcceptGateway: st.Topology == "gateway",
		ReplyVia: st.ReplyVia != "", Router: st.ReplyVia, PanelPort: st.PanelPort}
	if c.LAN == nil {
		c.LAN = []string{}
	}
	var nets []string
	for _, r := range st.ClientRoutes {
		c.LANGateway = r.Via
		nets = append(nets, r.Net)
	}
	c.ClientNets = strings.Join(nets, "\n")
	return c
}
