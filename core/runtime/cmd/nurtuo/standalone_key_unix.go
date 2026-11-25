// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
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

// Serialize the existence check and rename across processes on volumes without
// hard links. Keep the lock file in place so every caller locks the same inode.
func publishSigningKey(staged, path string) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(staged, path)
}
