package main

import (
	"os"
	"path/filepath"
)

// Paths locates everything the installer manages. Everything lives under Root (/opt/atom) except the
// systemd unit files; ATOM_ROOT and ATOM_UNIT_DIR exist for the tests.
type Paths struct {
	Root    string
	UnitDir string
	OpenRC  bool // OpenRC service scripts in UnitDir (/etc/init.d) instead of systemd units
}

func defaultPaths() Paths {
	p := Paths{Root: "/opt/atom", UnitDir: "/etc/systemd/system", OpenRC: useOpenRC()}
	if p.OpenRC {
		p.UnitDir = "/etc/init.d"
	}
	if v := os.Getenv("ATOM_ROOT"); v != "" {
		p.Root = v
	}
	if v := os.Getenv("ATOM_UNIT_DIR"); v != "" {
		p.UnitDir = v
	}
	return p
}

func (p Paths) Bin(name string) string { return filepath.Join(p.Root, "bin", name) }
func (p Paths) Etc(name string) string { return filepath.Join(p.Root, "etc", name) }
func (p Paths) Lib(name string) string { return filepath.Join(p.Root, "lib", name) }
func (p Paths) Run(name string) string { return filepath.Join(p.Root, "run", name) }
func (p Paths) Data() string           { return filepath.Join(p.Root, "data") }
func (p Paths) Backup() string         { return filepath.Join(p.Root, "backup") }
func (p Paths) Pkg() string            { return filepath.Join(p.Root, "pkg") }
func (p Paths) State() string          { return p.Etc("install.json") }
func (p Paths) Changes() string        { return p.Etc("changes.json") }
func (p Paths) DNSUnit() string        { return p.unitFile(dnsUnitName) }
func (p Paths) SingBoxUnit() string    { return p.unitFile(singBoxUnitName) }

// unitFile is the service definition: <name>.service for systemd, the script <name> for OpenRC.
func (p Paths) unitFile(name string) string {
	if p.OpenRC {
		name = rcName(name)
	}
	return filepath.Join(p.UnitDir, name)
}

func (p Paths) Init() initSystem {
	if p.OpenRC {
		return openRCInit{}
	}
	return systemdInit{}
}

// unitMode is the mode of a service definition: OpenRC scripts are programs.
func (p Paths) unitMode() os.FileMode {
	if p.OpenRC {
		return 0o755
	}
	return 0o644
}

const (
	dnsUnitName     = "atom-dns.service"
	singBoxUnitName = "atom-sing-box.service"
	defaultPort     = 9996
	fwmark          = "0x233"
	routeTable      = "233"
	routePriority   = "9233"
	// the single-NIC option that sends the answers to the LAN through the main router (deploy-nft.md 3.4)
	replyMark     = "0x234"
	replyTable    = "234"
	replyPriority = "9234"
)
