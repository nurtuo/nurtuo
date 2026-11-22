// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"io"
)

const flowUsage = `Usage: nurtuo flow <action>

Actions:
  run <flow.json>     run a canvas-exported flow in-process against connection files
`

func init() { register("flow", flowMain) }

// flowMain dispatches "nurtuo flow <action>"; only "run" exists.
func flowMain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "run" {
		_, _ = fmt.Fprint(stderr, flowUsage)
		return exitUsage
	}
	return flowRun(args[1:], stdout, stderr, nativeConnectors())
}
