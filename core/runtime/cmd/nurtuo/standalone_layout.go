// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// DataDirs is the on-disk layout of a standalone install.
type DataDirs struct {
	Root     string // base directory
	Postgres string // PostgreSQL data cluster
	PGRun    string // postgres runtime/extracted binaries
	PGCache  string // downloaded binary cache
	Logs     string
	Secrets  string // CA, certs, tokens (0700)
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
	}
}

// Ensure creates all directories; Secrets is private.
func (d DataDirs) Ensure() error {
	for _, p := range []string{d.Postgres, d.PGRun, d.PGCache, d.Logs} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}
	return os.MkdirAll(d.Secrets, 0o700)
}

func defaultDataRoot() (string, error) { return DefaultDataRoot(runtime.GOOS, os.Getenv) }
