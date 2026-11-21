// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package secrets

import "io/fs"

// checkPerm is a no-op on Windows: POSIX mode bits do not express ACLs, and secret directories
// inherit the owner's profile ACL. Documented gap in ADR 0010 (docs/adr/0010-connections-and-secrets.md).
func checkPerm(string, fs.FileInfo) error { return nil }

func chmodPrivate(string) error { return nil }
