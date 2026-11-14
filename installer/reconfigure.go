package main

import (
	"fmt"
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

// restore puts the old configuration back after a failed reconfigure: the files, then the services (their
// start step loads the old nft rules and routing again), then whatever the new state added that the old one
// did not have. It never takes the network down.
func (rc *reconfigureRun) restore(ins *Installer, log Logger) {
	log.Logf("恢复备份")
	newCh := loadChanges(ins.Paths)
	if err := ins.restoreBackup(rc.dir, rc.oldSt.PanelPort, log); err != nil {
		log.Logf("恢复备份出错：%v（备份在 %s）", err, rc.dir)
	}
	rc.undoAdditions(newCh, log)
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
