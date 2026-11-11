package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data, mode)
}

// productOwned names the files of the shipped configuration an upgrade overwrites; every other file of the
// configuration root (webinfo/, rules and lists that the daily updates replace, caches) is data and stays.
func productOwned(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == "config_custom.yaml" || strings.HasPrefix(rel, "sub_config/") || rel == "rule/atom_catalog.json"
}

// refreshData lays the shipped configuration tree (pkgConfig) over the configuration root: product files are
// replaced, everything else is copied only when missing. config_custom.yaml gets the panel port.
func refreshData(pkgConfig, dataDir string, panelPort int) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(pkgConfig, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(pkgConfig, path)
		if err != nil || rel == "." {
			return err
		}
		dst := filepath.Join(dataDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() || rel == "README.md" {
			return nil
		}
		if rel == "config_custom.yaml" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out, err := setPanelPort(string(data), panelPort)
			if err != nil {
				return err
			}
			return writeFileAtomic(dst, []byte(out), 0o644)
		}
		if !productOwned(rel) {
			if _, err := os.Lstat(dst); err == nil {
				return nil
			}
		}
		return copyFileAtomic(path, dst, 0o644)
	})
}

// extractTarGz unpacks a .tar.gz into dir, refusing anything that would land outside it.
func extractTarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root := filepath.Clean(dir) + string(os.PathSeparator)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, h.Name)
		if target != filepath.Clean(dir) && !strings.HasPrefix(target, root) {
			return fmt.Errorf("archive entry %q leaves the target directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// links and devices are not part of a WebUI package
		}
	}
}

// installWebUI replaces <data>/ui/main with the contents of the WebUI package.
func installWebUI(archive, dataDir string) error {
	ui := filepath.Join(dataDir, "ui")
	if err := os.MkdirAll(ui, 0o755); err != nil {
		return err
	}
	next := filepath.Join(ui, ".main.new")
	os.RemoveAll(next)
	if err := os.MkdirAll(next, 0o755); err != nil {
		return err
	}
	if err := extractTarGz(archive, next); err != nil {
		os.RemoveAll(next)
		return err
	}
	if _, err := os.Stat(filepath.Join(next, "index.html")); err != nil {
		os.RemoveAll(next)
		return fmt.Errorf("the WebUI package has no index.html")
	}
	main := filepath.Join(ui, "main")
	old := filepath.Join(ui, ".main.old")
	os.RemoveAll(old)
	if _, err := os.Lstat(main); err == nil {
		if err := os.Rename(main, old); err != nil {
			return err
		}
	}
	if err := os.Rename(next, main); err != nil {
		os.Rename(old, main)
		return err
	}
	os.RemoveAll(old)
	return nil
}

// copyTree copies dir src to dst (regular files and directories only).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFileAtomic(path, target, info.Mode().Perm())
	})
}
