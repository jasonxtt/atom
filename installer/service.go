package main

import (
	"os"
	"strings"
	"time"
)

// initSystem is what the installer needs of the service manager, for atom's own two services and for the
// services it stops to free a port. Names may be given with or without the systemd ".service" suffix.
type initSystem interface {
	Reload(log Logger) error // pick up changed service files
	Enable(log Logger, names ...string) error
	DisableNow(log Logger, names ...string) error
	Start(log Logger, name string) error
	Stop(log Logger, name string) error
	Restart(log Logger, name string) error
	IsActive(name string) string // "active" when running
	Logs(name string) string     // the last lines of its log
	Forget(log Logger, names ...string)
}

// useOpenRC says which service manager this machine has: systemd when it runs, else OpenRC. ATOM_INIT
// (systemd or openrc) exists for the tests.
func useOpenRC() bool {
	switch os.Getenv("ATOM_INIT") {
	case "openrc":
		return true
	case "systemd":
		return false
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return false
	}
	if _, err := os.Stat("/run/openrc"); err == nil {
		return true
	}
	_, err := os.Stat("/sbin/openrc-run")
	return err == nil
}

type systemdInit struct{}

func sysdName(n string) string {
	if !strings.HasSuffix(n, ".service") {
		return n + ".service"
	}
	return n
}

func sysdNames(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = sysdName(n)
	}
	return out
}

func (systemdInit) Reload(log Logger) error {
	return runCmd(log, 30*time.Second, "systemctl", "daemon-reload")
}

func (systemdInit) Enable(log Logger, names ...string) error {
	return runCmd(log, 30*time.Second, "systemctl", append([]string{"enable"}, sysdNames(names)...)...)
}

func (systemdInit) DisableNow(log Logger, names ...string) error {
	return runCmd(log, 60*time.Second, "systemctl", append([]string{"disable", "--now"}, sysdNames(names)...)...)
}

func (systemdInit) Start(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "systemctl", "start", sysdName(name))
}

func (systemdInit) Stop(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "systemctl", "stop", sysdName(name))
}

func (systemdInit) Restart(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "systemctl", "restart", sysdName(name))
}

func (systemdInit) IsActive(name string) string {
	out, _ := quietCmd("systemctl", "is-active", sysdName(name))
	return out
}

// IsEnabled is for the services of others that the wizard stops (free.go).
func (systemdInit) IsEnabled(name string) bool {
	out, _ := quietCmd("systemctl", "is-enabled", sysdName(name))
	return strings.HasPrefix(out, "enabled")
}

func (systemdInit) Logs(name string) string {
	out, _ := quietCmd("journalctl", "-u", sysdName(name), "-n", "15", "--no-pager")
	return out
}

func (systemdInit) Forget(log Logger, names ...string) {
	_ = runCmd(log, 30*time.Second, "systemctl", "daemon-reload")
	_ = runCmd(log, 30*time.Second, "systemctl", append([]string{"reset-failed"}, sysdNames(names)...)...)
}

// openRCInit runs atom's services in the default runlevel. The log of each is /var/log/atom/<name>.log.
type openRCInit struct{}

var openRCLogDir = "/var/log/atom" // a variable for the tests

func rcName(n string) string { return strings.TrimSuffix(n, ".service") }

func (openRCInit) Reload(Logger) error { return nil }

func (openRCInit) Enable(log Logger, names ...string) error {
	for _, n := range names {
		if err := runCmd(log, 30*time.Second, "rc-update", "add", rcName(n), "default"); err != nil {
			return err
		}
	}
	return nil
}

func (o openRCInit) DisableNow(log Logger, names ...string) error {
	var first error
	for _, n := range names {
		if err := o.Stop(log, n); err != nil && first == nil {
			first = err
		}
		if err := runCmd(log, 30*time.Second, "rc-update", "del", rcName(n), "default"); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (openRCInit) Start(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "rc-service", rcName(name), "start")
}

func (openRCInit) Stop(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "rc-service", rcName(name), "stop")
}

func (openRCInit) Restart(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "rc-service", rcName(name), "restart")
}

func (openRCInit) IsActive(name string) string {
	if _, err := quietCmd("rc-service", rcName(name), "status"); err == nil {
		return "active"
	}
	return "inactive"
}

func (openRCInit) Logs(name string) string {
	out, _ := quietCmd("tail", "-n", "15", openRCLogDir+"/"+rcName(name)+".log")
	return out
}

func (openRCInit) Forget(Logger, ...string) {}
