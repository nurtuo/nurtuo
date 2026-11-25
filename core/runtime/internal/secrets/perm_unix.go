// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package secrets

import (
	"fmt"
	"io/fs"
	"os"
)

// checkPerm refuses group/other-accessible secret files and directories.
func checkPerm(path string, fi fs.FileInfo) error {
	if m := fi.Mode().Perm(); m&0o077 != 0 {
		return fmt.Errorf("%s has permissions %#o; group/other access is not allowed (use chmod 600 for files, 700 for directories)", path, m)
	}
	return nil
}

func chmodPrivate(path string) error { return os.Chmod(path, 0o600) }

// createPrivateExclusive creates a new 0600 file and fails if it exists.
func createPrivateExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// mkdirPrivate creates path (and missing parents) with mode 0700; an existing directory is left as it is.
func mkdirPrivate(path string) error { return os.MkdirAll(path, 0o700) }

// EnsurePrivateDir creates path (0700 on Unix, protected private DACL on Windows) and, on
// Windows, verifies and repairs the directory and everything below it. It returns the paths whose
// DACL it tightened. It fails closed if any object has a foreign owner or cannot be fixed.
// Only for Nurtuo-managed paths (standalone secrets\, #64 bundles\); never for operator-chosen paths (D9).
//
// On Unix it is os.MkdirAll(path, 0o700) and never re-modes an existing directory (unchanged, D8).
//
// TODO(#78): interim placement in internal/secrets (D1); see perm_windows.go.
func EnsurePrivateDir(path string) ([]string, error) { return nil, os.MkdirAll(path, 0o700) }

// ensurePrivateDirNoRepair is the check-only counterpart for operator-chosen paths; on Unix it is
// os.MkdirAll(path, 0o700) as before (the read paths check the mode bits).
func ensurePrivateDirNoRepair(path string) error { return os.MkdirAll(path, 0o700) }

// checkOperatorPaths is a no-op on Unix: the read paths check the mode bits (unchanged, D8).
func checkOperatorPaths(Config) error { return nil }

// checkStoreDir is a no-op on Unix (unchanged, D8).
func checkStoreDir(string) error { return nil }
