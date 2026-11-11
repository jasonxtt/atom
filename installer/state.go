package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InstallState is etc/install.json: what was installed and how, so that an upgrade renders the same files
// and an uninstall knows what to take away. Complete turns true only when an installation finished.
type InstallState struct {
	Version     string    `json:"version"`
	Complete    bool      `json:"complete"`
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Topology    string    `json:"topology"` // bypass | gateway
	LAN         []string  `json:"lan"`
	WAN         string    `json:"wan,omitempty"`
	PanelPort   int       `json:"panel_port"`
	NftBin      string    `json:"nft_bin"`
	SysctlBin   string    `json:"sysctl_bin"`
}

// Changes is etc/changes.json: what the installer changed on the system outside /opt/atom (and the unit
// files), recorded when it happens so that an uninstall can put it back.
type Changes struct {
	ResolvedDropIn  bool        `json:"resolved_dropin,omitempty"`
	ResolvConfLink  string      `json:"resolv_conf_link,omitempty"` // the symlink target /etc/resolv.conf had
	DisabledUnits   []UnitState `json:"disabled_units,omitempty"`
	IPForwardBefore string      `json:"ip_forward_before,omitempty"`
	AptInstalled    []string    `json:"apt_installed,omitempty"`
}

// UnitState is a service the installer stopped to free a port, as it was before.
type UnitState struct {
	Unit       string `json:"unit"`
	WasEnabled bool   `json:"was_enabled"`
	WasActive  bool   `json:"was_active"`
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o644)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func loadState(p Paths) (*InstallState, error) {
	var st InstallState
	if err := readJSON(p.State(), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func loadChanges(p Paths) *Changes {
	var c Changes
	_ = readJSON(p.Changes(), &c)
	return &c
}

func saveChanges(p Paths, c *Changes) error { return writeJSON(p.Changes(), c) }

// writeFileAtomic writes next to the target and renames, so a reader never sees half a file and a running
// executable is replaced rather than overwritten.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
