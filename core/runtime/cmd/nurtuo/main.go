// SPDX-License-Identifier: AGPL-3.0-only

// Command nurtuo is the Nurtuo runtime: supervisor, engine, embedded NATS JetStream and
// native connectors in one static binary.
package main

import (
	"fmt"
	"os"

	"github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/core/runtime/internal/version"
)

const usage = `Usage: nurtuo <command>

Commands:
  run                 run the runtime in the foreground
  standalone          run runtime + control plane + embedded Postgres on one host
  service <action>    install | uninstall | start | stop the OS service
  role <name>         run a single role (distributed mode)
  version             print version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Printf("nurtuo %s (%s, %s)\n", version.Version, version.Commit, version.Date)
	case "run", "standalone", "service", "role":
		_ = http.New() // native connectors are registered here
		fmt.Fprintf(os.Stderr, "nurtuo %s: not implemented yet\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
