// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// Exit codes of "nurtuo flow run" (experimental contract; differs from nurtuoctl on purpose).
const (
	exitOK      = 0
	exitExec    = 1 // execution error: connector, failed document, no_runner, cancel, secret/host-key/auth
	exitUsage   = 2 // usage, flow or connection validation error
	exitStopped = 3 // the flow ended in a Stop step with a status
)

const flowRunUsage = "Usage: nurtuo flow run <flow.json> [--connection <file>]... [--set key=value]... [--secrets-dir <dir>] [--known-hosts <file>] [--insecure-host-key] [--json] [--timeout <duration>]"

// flowRunError is an error with its exit code and a machine-readable code for --json.
type flowRunError struct {
	exit int
	code string
	msg  string
}

func (e *flowRunError) Error() string { return e.msg }

func usageErr(format string, a ...any) *flowRunError {
	return &flowRunError{exit: exitUsage, code: "usage", msg: fmt.Sprintf(format, a...)}
}

func validationErr(format string, a ...any) *flowRunError {
	return &flowRunError{exit: exitUsage, code: "validation", msg: fmt.Sprintf(format, a...)}
}

func execErr(format string, a ...any) *flowRunError {
	return &flowRunError{exit: exitExec, code: "execution", msg: fmt.Sprintf(format, a...)}
}

// flowRunOptions are the parsed flags of "nurtuo flow run".
type flowRunOptions struct {
	flowPath    string
	connections []string
	set         map[string]string
	secretsDir  string
	knownHosts  string
	insecure    bool
	json        bool
	timeout     time.Duration
}

// parseFlowRunArgs parses flags and the single <flow.json> positional, in any order.
func parseFlowRunArgs(args []string) (*flowRunOptions, error) {
	o := &flowRunOptions{set: map[string]string{}}
	fs := flag.NewFlagSet("flow run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("connection", "", func(v string) error {
		if v == "" {
			return errors.New("empty path")
		}
		o.connections = append(o.connections, v)
		return nil
	})
	fs.Func("set", "", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return fmt.Errorf("%q is not key=value", v)
		}
		o.set[k] = val // duplicate key: last wins
		return nil
	})
	fs.StringVar(&o.secretsDir, "secrets-dir", "", "")
	fs.StringVar(&o.knownHosts, "known-hosts", "", "")
	fs.BoolVar(&o.insecure, "insecure-host-key", false, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.DurationVar(&o.timeout, "timeout", 0, "")
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return o, usageErr("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch {
	case len(positional) == 0:
		return o, usageErr("missing <flow.json>")
	case len(positional) > 1:
		return o, usageErr("expected one <flow.json>, got %d arguments", len(positional))
	case o.timeout < 0:
		return o, usageErr("--timeout must not be negative")
	}
	o.flowPath = positional[0]
	return o, nil
}

// wantsJSON reports whether --json appears in args, so usage errors can already be JSON.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "-json" || a == "--json=true" || a == "-json=true" {
			return true
		}
	}
	return false
}

// loadFlow reads and validates the flow file.
func loadFlow(path string) (*flow.Flow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, validationErr("read flow: %v", err)
	}
	f, diags, err := flow.Parse(raw)
	if err != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "invalid flow %s: %v", path, err)
		for _, d := range diags {
			fmt.Fprintf(&b, "\n  %s %s", d.Severity, d.Code)
			if d.Path != "" {
				fmt.Fprintf(&b, " %s", d.Path)
			}
			fmt.Fprintf(&b, ": %s", d.Message)
		}
		return nil, validationErr("%s", b.String())
	}
	return f, nil
}

// loadConnections parses every --connection file and rejects duplicate ids.
func loadConnections(paths []string) ([]*connections.Definition, error) {
	var defs []*connections.Definition
	seen := map[string]string{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, validationErr("read connection: %v", err)
		}
		d, err := connections.Parse(raw)
		if err != nil {
			return nil, validationErr("%s: %v", p, err)
		}
		if prev, dup := seen[d.ID]; dup {
			return nil, validationErr("connection %q is defined twice (%s and %s)", d.ID, prev, p)
		}
		seen[d.ID] = p
		defs = append(defs, d)
	}
	return defs, nil
}

// crossCheck matches every connector step to a connection definition by id (a step without an
// explicit connection uses the only definition of its connector, as connhost.Manager does) and
// returns the ids that are needed. Failures are validation errors raised before any network access.
func crossCheck(f *flow.Flow, defs []*connections.Definition, reg *connhost.Registry) ([]string, error) {
	byID := map[string]*connections.Definition{}
	byConnector := map[string][]string{}
	for _, d := range defs {
		byID[d.ID] = d
		byConnector[d.Connector] = append(byConnector[d.Connector], d.ID)
	}
	registered := map[string]bool{}
	for _, id := range reg.IDs() {
		registered[id] = true
	}
	needed := map[string]bool{}
	for _, s := range f.Steps {
		if s.Kind != flow.KindConnector {
			continue
		}
		c, err := s.TypedConfig()
		if err != nil {
			return nil, validationErr("step %s: %v", s.ID, err)
		}
		cc, ok := c.(flow.ConnectorConfig)
		if !ok {
			return nil, validationErr("step %s: unexpected config %T", s.ID, c)
		}
		if !registered[cc.Connector] {
			return nil, validationErr("step %s: connector %q is not available (available: %s)", s.ID, cc.Connector, strings.Join(reg.IDs(), ", "))
		}
		id := cc.Connection
		if id == "" {
			ids := byConnector[cc.Connector]
			switch len(ids) {
			case 0:
				return nil, validationErr("step %s: connector %q needs a connection; pass one with --connection", s.ID, cc.Connector)
			case 1:
				id = ids[0]
			default:
				sort.Strings(ids)
				return nil, validationErr("step %s: connector %q has several connections (%s); name one in the step's \"connection\"", s.ID, cc.Connector, strings.Join(ids, ", "))
			}
		}
		d, ok := byID[id]
		if !ok {
			return nil, validationErr("step %s references connection %q, which is not in the --connection files", s.ID, id)
		}
		if d.Connector != cc.Connector {
			return nil, validationErr("step %s: connection %q is for connector %q, not %q", s.ID, id, d.Connector, cc.Connector)
		}
		needed[id] = true
	}
	out := make([]string, 0, len(needed))
	for id := range needed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// reportError writes err to stderr (text or JSON) and returns its exit code. Stdout stays empty.
func reportError(stderr io.Writer, asJSON bool, err error) int {
	fe := &flowRunError{}
	if !errors.As(err, &fe) {
		fe = &flowRunError{exit: exitExec, code: "execution", msg: err.Error()}
	}
	if asJSON {
		b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": fe.code, "message": fe.msg}})
		_, _ = fmt.Fprintf(stderr, "%s\n", b)
	} else {
		_, _ = fmt.Fprintf(stderr, "nurtuo: %s\n", fe.msg)
	}
	return fe.exit
}

// flowRun implements "nurtuo flow run". reg is the connector registry (a test seam).
func flowRun(args []string, stdout, stderr io.Writer, reg *connhost.Registry) int {
	asJSON := wantsJSON(args)
	opts, err := parseFlowRunArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, flowRunUsage)
		return reportError(stderr, asJSON, err)
	}
	f, err := loadFlow(opts.flowPath)
	if err != nil {
		return reportError(stderr, opts.json, err)
	}
	defs, err := loadConnections(opts.connections)
	if err != nil {
		return reportError(stderr, opts.json, err)
	}
	if _, err := crossCheck(f, defs, reg); err != nil {
		return reportError(stderr, opts.json, err)
	}
	_ = stdout
	return reportError(stderr, opts.json, execErr("running a flow is not implemented yet"))
}
