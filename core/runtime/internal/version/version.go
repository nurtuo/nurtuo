// SPDX-License-Identifier: AGPL-3.0-only

// Package version holds build metadata injected by goreleaser via -ldflags.
package version

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
