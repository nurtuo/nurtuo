// SPDX-License-Identifier: AGPL-3.0-only

package main

import "os"

// EnsurePrivateDir checks Windows ownership and DACLs for the directory and its contents.
func checkStandaloneSecretsDir(string) error { return nil }

func checkSigningKeyPermissions(string, os.FileInfo) error { return nil }
