// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// SFTP host key policies (connectors/sftp settings.hostKeyPolicy).
const (
	sftpPolicyKnownHosts = "known_hosts"
	sftpPolicyInsecure   = "insecure"
)

// Exit codes of "nurtuo flow run" (experimental contract; differs from nurtuoctl on purpose).
const (
	exitOK      = 0
	exitExec    = 1 // execution error: connector, failed document, no_runner, cancel, secret/host-key/auth
	exitUsage   = 2 // usage, flow or connection validation error
	exitStopped = 3 // the flow ended in a Stop step with a status
)

const flowRunUsage = "Usage: nurtuo flow run <flow.json> [--connection <file>]... [--set key=value]... [--secrets-dir <dir>] [--known-hosts <file>] [--insecure-host-key] [--json] [--timeout <duration>] [--max-output-bytes N] [--max-documents N]"

// flowRunError is an error with its exit code and a machine-readable code for --json.
type flowRunError struct {
	exit int
	code string
	msg  string
	// steps is the per-step trace, set only when the engine ran (--json errors carry it).
	steps []stepOut
}

// stepOut is one entry of the additive "steps" array in the --json output (canvas-run spec D16).
type stepOut struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	In      int    `json:"in"`
	Out     int    `json:"out"`
	Failed  int    `json:"failed"`
	Dropped int    `json:"dropped"`
}

// aggregateSteps sums the engine trace per step id, in first-seen order (a step visited twice by
// two branches sums its counters). It returns a non-nil slice so the JSON key is always an array.
func aggregateSteps(trace []engine.StepTrace) []stepOut {
	out := []stepOut{}
	idx := map[string]int{}
	for _, t := range trace {
		i, ok := idx[t.StepID]
		if !ok {
			i = len(out)
			idx[t.StepID] = i
			out = append(out, stepOut{ID: t.StepID, Kind: t.Kind})
		}
		out[i].In += t.In
		out[i].Out += t.Out
		out[i].Failed += t.Failed
		out[i].Dropped += t.Dropped
	}
	return out
}

// withSteps returns a copy of err carrying the trace of res (when the engine produced one).
func withSteps(err *flowRunError, res *engine.Result) *flowRunError {
	cp := *err
	if res != nil {
		cp.steps = aggregateSteps(res.Trace)
	}
	return &cp
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
	flowPath       string
	connections    []string
	set            map[string]string
	secretsDir     string
	knownHosts     string
	insecure       bool
	json           bool
	timeout        time.Duration
	maxOutputBytes int64
	maxDocuments   int
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
	fs.Int64Var(&o.maxOutputBytes, "max-output-bytes", 0, "")
	fs.IntVar(&o.maxDocuments, "max-documents", 0, "")
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
	var badLimit bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-documents":
			badLimit = badLimit || o.maxDocuments <= 0
		case "max-output-bytes":
			badLimit = badLimit || o.maxOutputBytes <= 0 || o.maxOutputBytes > (1<<30)
		}
	})
	if badLimit {
		return o, usageErr("output limits must be positive and within range")
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
		body := map[string]any{"code": fe.code, "message": fe.msg}
		payload := map[string]any{"error": body}
		if fe.steps != nil {
			payload["steps"] = fe.steps
		}
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(stderr, "%s\n", b)
	} else {
		_, _ = fmt.Fprintf(stderr, "nurtuo: %s\n", fe.msg)
	}
	return fe.exit
}

// applyHostKeys applies the SFTP host-key options to the needed sftp connections in memory (the
// connection files are never modified). Verification stays on by default: the content of
// --known-hosts (default ~/.ssh/known_hosts) fills settings.knownHosts. --insecure-host-key is the
// only way to skip verification (it warns on stderr); a connection file saying "insecure" without
// the flag is a usage error. Precedence for known_hosts: --known-hosts, then inline knownHosts,
// then ~/.ssh/known_hosts. A fingerprint policy is left untouched.
func applyHostKeys(defs []*connections.Definition, needed []string, o *flowRunOptions, stderr io.Writer) error {
	byID := map[string]*connections.Definition{}
	for _, d := range defs {
		byID[d.ID] = d
	}
	var known string
	var haveKnown bool
	for _, id := range needed {
		d := byID[id]
		if d.Connector != "sftp" {
			continue
		}
		if d.Settings == nil {
			d.Settings = map[string]any{}
		}
		policy, _ := d.Settings["hostKeyPolicy"].(string)
		if o.insecure {
			d.Settings["hostKeyPolicy"] = sftpPolicyInsecure
			policy = sftpPolicyInsecure
		} else if policy == sftpPolicyInsecure {
			return validationErr("connection %q sets hostKeyPolicy: insecure; the only way to skip host-key verification is the --insecure-host-key flag", id)
		}
		switch policy {
		case sftpPolicyInsecure:
			if o.json {
				b, _ := json.Marshal(map[string]any{"warning": map[string]string{"code": "insecure_host_key", "message": fmt.Sprintf("host key verification is disabled for connection %q; the server is not authenticated", id)}})
				_, _ = fmt.Fprintf(stderr, "%s\n", b)
			} else {
				_, _ = fmt.Fprintf(stderr, "nurtuo: warning: host key verification is disabled for connection %q; the server is not authenticated\n", id)
			}
		case "", sftpPolicyKnownHosts:
			if o.knownHosts == "" {
				if kh, _ := d.Settings["knownHosts"].(string); kh != "" {
					continue // inline content from the connection file
				}
			}
			if !haveKnown {
				path := o.knownHosts
				if path == "" {
					home, err := os.UserHomeDir()
					if err != nil {
						return execErr("cannot find the default known_hosts file (%v); pass --known-hosts <file> or --insecure-host-key", err)
					}
					path = filepath.Join(home, ".ssh", "known_hosts")
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return execErr("cannot read known_hosts file: %v; pass --known-hosts <file> (or --insecure-host-key, not recommended)", err)
				}
				known, haveKnown = string(b), true
			}
			d.Settings["knownHosts"] = known
		}
	}
	return nil
}

// checkLiterals rejects literal values in secret-marked settings of the needed connections before
// any connector is configured (no network): the connector's connection schema says which settings
// are secret.
func checkLiterals(ctx context.Context, reg *connhost.Registry, defs []*connections.Definition, needed []string) error {
	byID := map[string]*connections.Definition{}
	for _, d := range defs {
		byID[d.ID] = d
	}
	for _, id := range needed {
		d := byID[id]
		c, err := reg.New(d.Connector)
		if err != nil {
			return validationErr("%v", err)
		}
		desc, derr := c.Describe(ctx)
		closeConnector(c)
		if derr != nil {
			return execErr("connection %q: describe %s: %v", id, d.Connector, derr)
		}
		if err := d.CheckLiterals(connections.SecretPaths(desc.GetConnectionSchema())); err != nil {
			return validationErr("%v; set it with the environment variable %s<NAME> or --secrets-dir", err, secrets.EnvPrefix)
		}
	}
	return nil
}

func closeConnector(c any) {
	switch x := c.(type) {
	case interface{ Close() error }:
		_ = x.Close()
	case interface{ Close() }:
		x.Close()
	}
}

// chainProvider resolves a secret from the first provider that has it.
type chainProvider []secrets.Provider

func (c chainProvider) Get(ctx context.Context, name string) (secrets.Secret, error) {
	var first error
	for _, p := range c {
		s, err := p.Get(ctx, name)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, secrets.ErrNotFound) {
			return secrets.Secret{}, err
		}
		if first == nil {
			first = err
		}
	}
	return secrets.Secret{}, first
}

// flowRun implements "nurtuo flow run". reg is the connector registry (a test seam).
func flowRun(args []string, stdout, stderr io.Writer, reg *connhost.Registry) int {
	asJSON := wantsJSON(args)
	opts, err := parseFlowRunArgs(args)
	if err != nil {
		var fe *flowRunError
		if errors.As(err, &fe) && asJSON {
			// stderr must stay one JSON object: the usage text goes into the message.
			cp := *fe
			cp.msg += "\n" + flowRunUsage
			return reportError(stderr, true, &cp)
		}
		_, _ = fmt.Fprintln(stderr, flowRunUsage)
		return reportError(stderr, asJSON, err)
	}
	redactor := secrets.NewRedactor()
	fail := func(err error) int {
		var fe *flowRunError
		if errors.As(err, &fe) {
			// Redact in place: RedactError would drop the chain (and with it the exit code).
			cp := *fe
			cp.msg = redactor.Redact(cp.msg)
			return reportError(stderr, opts.json, &cp)
		}
		return reportError(stderr, opts.json, redactor.RedactError(err))
	}
	f, err := loadFlow(opts.flowPath)
	if err != nil {
		return fail(err)
	}
	defs, err := loadConnections(opts.connections)
	if err != nil {
		return fail(err)
	}
	needed, err := crossCheck(f, defs, reg)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}
	if err := applyHostKeys(defs, needed, opts, stderr); err != nil {
		return fail(err)
	}
	if err := checkLiterals(ctx, reg, defs, needed); err != nil {
		return fail(err)
	}

	var provider chainProvider = []secrets.Provider{secrets.NewEnv()}
	if opts.secretsDir != "" {
		provider = append(provider, secrets.NewDir(opts.secretsDir))
	}
	mgr := connhost.NewManager(reg, &connections.Resolver{Secrets: provider, Redactor: redactor})
	defer mgr.Close()
	prepared, err := mgr.Prepare(ctx, "flow-run", []*flow.Flow{f}, defs)
	if err != nil {
		return fail(execErr("%v", err))
	}
	mgr.Commit(prepared)

	eng := engine.New()
	eng.MaxReturnedDocuments = opts.maxDocuments
	eng.MaxReturnedBytes = opts.maxOutputBytes
	if opts.maxOutputBytes > 0 {
		eng.Eval = engine.NewBoundedEvaluator(opts.maxOutputBytes)
		eng.Spool = docs.NewSpool(docs.SpoolConfig{DiskBudget: opts.maxOutputBytes})
	}
	eng.Registry.RegisterConnectorLookup(mgr) // start is a pass-through when no runner is registered
	in := docs.NewSet(docs.New("flow-run", opts.set, nil))
	res, err := eng.Run(ctx, f, in, engine.NewProcess(nil))
	if res != nil {
		defer func() { _ = res.Returned.Close(); _ = res.Failed.Close() }()
	}
	if eng.Spool.DiskBudgetExceeded() || errors.Is(err, engine.ErrReturnLimit) {
		return fail(withSteps(outputTooLarge(), res))
	}
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fail(withSteps(execErr("timed out after %s", opts.timeout), res))
		case errors.Is(err, context.Canceled):
			return fail(withSteps(execErr("interrupted"), res))
		}
		return fail(withSteps(execErr("%v", err), res))
	}
	if n := res.Failed.Len(); n > 0 {
		var msgs []string
		for _, d := range res.Failed.Docs() {
			if e := d.Err(); e != nil {
				m := e.Error()
				if p := d.Props["path"]; p != "" {
					m += " (path " + p + ")" // the document's path property, as set with --set path=...
				}
				msgs = append(msgs, m)
			}
		}
		return fail(withSteps(execErr("%d document(s) failed: %s", n, strings.Join(msgs, "; ")), res))
	}
	if res.Stop != nil && res.Stop.Status != 0 {
		fe := &flowRunError{exit: exitStopped, code: "stopped", msg: fmt.Sprintf("flow stopped with status %d", res.Stop.Status)}
		if res.Stop.Message != "" {
			fe.msg += ": " + res.Stop.Message
		}
		return fail(withSteps(fe, res))
	}
	if err := writeOutput(stdout, res, opts); err != nil {
		if errors.Is(err, errOutputTooLarge) {
			return fail(withSteps(outputTooLarge(), res))
		}
		return fail(execErr("write output: %v", err))
	}
	return exitOK
}

var errOutputTooLarge = errors.New("output exceeds the byte or document limit")

func outputTooLarge() *flowRunError {
	return &flowRunError{exit: exitExec, code: "output_too_large", msg: errOutputTooLarge.Error()}
}

// writeOutput prints the returned documents: raw payload bytes, or the JSON envelope.
func writeOutput(stdout io.Writer, res *engine.Result, opts *flowRunOptions) error {
	ret := res.Returned
	if opts.maxDocuments > 0 && ret.Len() > opts.maxDocuments {
		return errOutputTooLarge
	}
	if opts.maxOutputBytes > 0 {
		var total int64
		for i := 0; i < ret.Len(); i++ {
			if p := ret.At(i).Payload(); p != nil {
				if p.Size() > opts.maxOutputBytes-total {
					return errOutputTooLarge
				}
				total += p.Size()
			}
		}
	}
	if opts.json {
		type docOut struct {
			ID         string            `json:"id"`
			Properties map[string]string `json:"properties"`
			Payload    string            `json:"payload"` // base64
		}
		out := struct {
			Status    int       `json:"status"`
			Documents []docOut  `json:"documents"`
			Steps     []stepOut `json:"steps"`
		}{Status: 200, Documents: []docOut{}, Steps: aggregateSteps(res.Trace)}
		var wireBytes int64
		if opts.maxOutputBytes > 0 {
			envelope, err := json.Marshal(out)
			if err != nil {
				return err
			}
			wireBytes = int64(len(envelope)) + 1 // terminating newline
		}
		// The decoded limit remains available for payload. Wire overhead, including
		// metadata and JSON escapes, is independently bounded.
		wireLimit := opts.maxOutputBytes + (opts.maxOutputBytes+2)/3 + 128*1024
		for _, d := range ret.Docs() {
			props := d.Props
			if props == nil {
				props = map[string]string{}
			}
			if opts.maxOutputBytes > 0 {
				// Reject oversized metadata before allocating an escaped JSON copy.
				metadataBytes := int64(len(d.ID))
				for key, value := range props {
					metadataBytes += int64(len(key)) + int64(len(value))
				}
				if metadataBytes > wireLimit-wireBytes {
					return errOutputTooLarge
				}
				wireBytes++ // document separator
				metadata, err := json.Marshal(docOut{ID: d.ID, Properties: props})
				if err != nil {
					return err
				}
				wireBytes += int64(len(metadata))
				if p := d.Payload(); p != nil {
					wireBytes += 4 * ((p.Size() + 2) / 3)
				}
				if wireBytes > wireLimit {
					return errOutputTooLarge
				}
			}
			var b []byte
			if p := d.Payload(); p != nil {
				var err error
				if b, err = p.Bytes(); err != nil {
					return err
				}
			}
			out.Documents = append(out.Documents, docOut{ID: d.ID, Properties: props, Payload: base64.StdEncoding.EncodeToString(b)})
		}
		enc := json.NewEncoder(stdout)
		return enc.Encode(out)
	}
	switch ret.Len() {
	case 0:
		return nil
	case 1:
		r, err := ret.At(0).Open()
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		_, err = io.Copy(stdout, r)
		return err
	}
	return fmt.Errorf("the flow returned %d documents; use --json to print them", ret.Len())
}
