// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
)

// DataDirs is the on-disk layout of a standalone install.
type DataDirs struct {
	Root     string // base directory
	Postgres string // PostgreSQL data cluster
	PGRun    string // postgres runtime/extracted binaries
	PGCache  string // downloaded binary cache
	Logs     string
	Secrets  string // CA, certs, tokens (0700 / private DACL on Windows)
	Bundles  string // verified bundle cache (0700 / private DACL on Windows)
}

// DefaultDataRoot returns the per-OS default data directory. env supplies lookups
// (os.Getenv in production) so the function is testable.
func DefaultDataRoot(goos string, env func(string) string) (string, error) {
	if v := env("NURTUO_DATA_DIR"); v != "" {
		return v, nil
	}
	home := env("HOME")
	switch goos {
	case "windows":
		if v := env("ProgramData"); v != "" {
			return filepath.Join(v, "Nurtuo"), nil
		}
		if v := env("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "Nurtuo"), nil
		}
	case "darwin":
		if home != "" {
			return filepath.Join(home, "Library", "Application Support", "Nurtuo"), nil
		}
	default:
		if v := env("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "nurtuo"), nil
		}
		if home != "" {
			return filepath.Join(home, ".local", "share", "nurtuo"), nil
		}
	}
	return "", fmt.Errorf("cannot determine data directory for %s; set NURTUO_DATA_DIR", goos)
}

// NewDataDirs computes the layout under root.
func NewDataDirs(root string) DataDirs {
	return DataDirs{
		Root:     root,
		Postgres: filepath.Join(root, "postgres", "data"),
		PGRun:    filepath.Join(root, "postgres", "runtime"),
		PGCache:  filepath.Join(root, "postgres", "cache"),
		Logs:     filepath.Join(root, "logs"),
		Secrets:  filepath.Join(root, "secrets"),
		Bundles:  filepath.Join(root, "bundles"),
	}
}

// Ensure creates all directories, including the private secrets and bundles directories.
func (d DataDirs) Ensure() error {
	_, err := d.EnsureReport()
	return err
}

// EnsureReport creates the layout and reports paths whose Windows DACLs were tightened.
// The bundle cache is also tightened to 0700 on Unix.
func (d DataDirs) EnsureReport() ([]string, error) {
	for _, p := range []string{d.Postgres, d.PGRun, d.PGCache, d.Logs} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, err
		}
	}
	repairedSecrets, err := secrets.EnsurePrivateDir(d.Secrets)
	if err != nil {
		return repairedSecrets, err
	}
	repairedBundles, err := secrets.EnsurePrivateDir(d.Bundles)
	if err == nil && runtime.GOOS != "windows" {
		err = tightenBundleDir(d.Bundles)
	}
	return append(repairedSecrets, repairedBundles...), err
}

func tightenBundleDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: bundle cache must be a directory", path)
	}
	return os.Chmod(path, 0o700)
}

// ensureDataDirs runs EnsureReport and prints every path whose permissions were tightened.
func ensureDataDirs(d DataDirs, stderr io.Writer) error {
	repaired, err := d.EnsureReport()
	for _, p := range repaired {
		_, _ = fmt.Fprintln(stderr, "standalone: tightened permissions on", p)
	}
	return err
}

func defaultDataRoot() (string, error) { return DefaultDataRoot(runtime.GOOS, os.Getenv) }
