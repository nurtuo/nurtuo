// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"io"
)

const flowUsage = `Usage: nurtuo flow <action>

Actions:
  host-key <host>   scan the SSH host key negotiated by SFTP
  run <flow.json>     run a canvas-exported flow in-process against connection files
`

func init() { register("flow", flowMain) }

// flowMain dispatches "nurtuo flow <action>".
func flowMain(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "host-key" {
		return flowHostKey(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "run-pinned" {
		registry, err := pinnedConnectors()
		if err != nil {
			return reportError(stderr, wantsJSON(args[1:]), execErr("test run dial enforcement unavailable"))
		}
		return flowRun(args[1:], stdout, stderr, registry)
	}
	if len(args) < 1 || args[0] != "run" {
		_, _ = fmt.Fprint(stderr, flowUsage)
		return exitUsage
	}
	return flowRun(args[1:], stdout, stderr, nativeConnectors())
}
