// SPDX-License-Identifier: AGPL-3.0-only

// Command nurtuo is the Nurtuo runtime: supervisor, engine, embedded NATS JetStream and
// native connectors in one static binary.
package main

import (
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/nurtuo/nurtuo/core/runtime/internal/version"
)

const usage = `Usage: nurtuo <command>

Commands:
  run                 run the runtime in the foreground
  flow run <file>     run a canvas-exported flow in-process (no control plane)
  standalone          run runtime + control plane + embedded Postgres on one host
  service <action>    install | uninstall | start | stop | status | run the OS service
  secret <action>     set | get | list | delete secrets used by bundle connections
  role <name>         run a single role (distributed mode)
  version             print version
`

// commandFunc runs a subcommand with the arguments after its name and returns the exit code.
type commandFunc func(args []string, stdout, stderr io.Writer) int

// commands is the subcommand registry. Each subcommand registers itself from an init()
// in its own file (service.go, standalone.go, ...), so files never need to touch main.go.
var commands = map[string]commandFunc{}

func register(name string, fn commandFunc) {
	if _, dup := commands[name]; dup {
		panic("nurtuo: duplicate command " + name)
	}
	commands[name] = fn
}

// stubs are documented commands that are not implemented yet.
var stubs = []string{"role"}

func main() { os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr)) }

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	name := args[0]
	if name == "version" {
		_, _ = fmt.Fprintf(stdout, "nurtuo %s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return 0
	}
	if fn, ok := commands[name]; ok {
		return fn(args[1:], stdout, stderr)
	}
	if slices.Contains(stubs, name) {
		_, _ = fmt.Fprintf(stderr, "nurtuo %s: not implemented yet\n", name)
		return 1
	}
	_, _ = fmt.Fprint(stderr, usage)
	return 2
}
