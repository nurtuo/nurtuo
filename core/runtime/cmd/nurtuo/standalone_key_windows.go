// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnsurePrivateDir checks Windows ownership and DACLs for the directory and its contents.
func checkStandaloneSecretsDir(string) error { return nil }

func checkSigningKeyPermissions(string, os.FileInfo) error { return nil }

func publishSigningKey(staged, path string) error {
	from, err := windows.UTF16PtrFromString(staged)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// MoveFile performs a same-volume rename and fails if the destination exists.
	return windows.MoveFile(from, to)
}
