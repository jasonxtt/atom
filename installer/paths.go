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
}

func defaultPaths() Paths {
	p := Paths{Root: "/opt/atom", UnitDir: "/etc/systemd/system"}
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
func (p Paths) DNSUnit() string        { return filepath.Join(p.UnitDir, dnsUnitName) }
func (p Paths) SingBoxUnit() string    { return filepath.Join(p.UnitDir, singBoxUnitName) }

const (
	dnsUnitName     = "atom-dns.service"
	singBoxUnitName = "atom-sing-box.service"
	defaultPort     = 9996
	fwmark          = "0x233"
	routeTable      = "233"
	routePriority   = "9233"
)
