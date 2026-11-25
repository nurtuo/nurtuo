// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

func checkStandaloneSecretsDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s: secrets directory must be a real directory with mode 0700", path)
	}
	return checkStandaloneOwner(path, info)
}

func checkSigningKeyPermissions(path string, info os.FileInfo) error {
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s: bundle signing key must have mode 0600", path)
	}
	return checkStandaloneOwner(path, info)
}

func checkStandaloneOwner(path string, info os.FileInfo) error {
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s: must be owned by the effective user", path)
	}
	return nil
}
