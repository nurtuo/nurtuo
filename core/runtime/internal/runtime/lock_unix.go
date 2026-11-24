// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package runtime

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking advisory lock on f.
func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
