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
	Runtime  string // runtime state_dir: queue data and applied revision (private)
	Secrets  string // CA, certs, tokens (0700 / private DACL on Windows)
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
		Runtime:  filepath.Join(root, "runtime"),
		Secrets:  filepath.Join(root, "secrets"),
	}
}

// Ensure creates all directories; Secrets is private (see EnsureReport).
func (d DataDirs) Ensure() error {
	_, err := d.EnsureReport()
	return err
}

// EnsureReport creates all directories and makes Secrets private: 0700 on Unix; on Windows a
// protected DACL for this account, SYSTEM and Administrators, repairing an existing too-open
// secrets tree and failing closed on a foreign owner (secrets.EnsurePrivateDir). It returns the
// paths whose permissions were tightened (always none on Unix).
func (d DataDirs) EnsureReport() ([]string, error) {
	for _, p := range []string{d.Postgres, d.PGRun, d.PGCache, d.Logs} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, err
		}
	}
	return secrets.EnsurePrivateDir(d.Secrets)
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
