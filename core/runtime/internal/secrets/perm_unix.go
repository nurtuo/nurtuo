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
