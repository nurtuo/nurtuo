// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestBootstrapRejectsUnsafeSecretsDirectory(t *testing.T) {
	for _, kind := range []string{"group writable", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dirs := NewDataDirs(t.TempDir())
			target := dirs.Secrets
			if kind == "symlink" {
				target = filepath.Join(dirs.Root, "target")
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(target, dirs.Secrets); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(target, 0o770); err != nil {
				t.Fatal(err)
			}
			if _, err := NewLocalBootstrapper().Bootstrap(dirs); err == nil || !strings.Contains(err.Error(), "mode 0700") {
				t.Fatalf("Bootstrap returned %v", err)
			}
			if _, err := os.Lstat(filepath.Join(target, "bundle-signing.key")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bootstrap touched the signing key: %v", err)
			}
		})
	}
}

type signingKeyFileInfo struct {
	os.FileInfo
	owner syscall.Stat_t
}

func (info signingKeyFileInfo) Sys() any { return &info.owner }

func TestRejectForeignOwnedSigningKey(t *testing.T) {
	dirs := NewDataDirs(t.TempDir())
	creds, err := NewLocalBootstrapper().Bootstrap(dirs)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.Lstat(creds.BundleSigningKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	foreign := signingKeyFileInfo{FileInfo: key, owner: syscall.Stat_t{Uid: uint32(os.Geteuid()) + 1}}
	if err := checkSigningKeyPermissions(creds.BundleSigningKeyFile, foreign); err == nil || !strings.Contains(err.Error(), "owned by the effective user") {
		t.Fatalf("foreign key owner check returned %v", err)
	}
}
