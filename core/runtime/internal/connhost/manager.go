// SPDX-License-Identifier: AGPL-3.0-only

// Package connhost holds the registry of native connectors and the Manager that owns connector
// instances per deployment: resolve secrets, create, Test on deploy, Close on undeploy.
package connhost

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Test modes (runtime config `connection_test`).
const (
	TestStrict = "strict" // a failing Test fails the deployment (default)
	TestWarn   = "warn"   // a failing Test is reported in the status message; deploy proceeds
	TestOff    = "off"    // connections are not tested
)

// DefaultTestTimeout bounds one connection Test.
const DefaultTestTimeout = 15 * time.Second

// Manager creates and owns connector instances, one per (deployment, connection).
type Manager struct {
	Registry *Registry
	Resolver *connections.Resolver
	Redactor *secrets.Redactor // scrubs diagnostics, errors and results; may be nil
	TestMode string            // strict (default) | warn | off
	Timeout  time.Duration     // per-connection Test timeout

	mu      sync.Mutex
	deploys map[string]*Prepared // deployment -> live instances
	flows   map[string]string    // flow id -> deployment
}

// NewManager returns a manager; the resolver's redactor is shared when r.Redactor is nil.
func NewManager(reg *Registry, res *connections.Resolver) *Manager {
	m := &Manager{Registry: reg, Resolver: res, deploys: map[string]*Prepared{}, flows: map[string]string{}}
	if res != nil {
		m.Redactor = res.Redactor
	}
	return m
}

type instance struct {
	id, connectorID string
	def             *connections.Definition
	secretPaths     map[string]bool
	conn            connector.Connector
	cfg             *connectorv1.ConnectionConfig
	tested          string // human status: "ok", "warning: ...", "not testable", "untested"
}

// Prepared is a staged set of instances for one deployment. Commit it or Discard it.
type Prepared struct {
	deployment string
	flowIDs    []string
	byID       map[string]*instance
	defaults   map[string]string // connector id -> default connection id
}

// Summary is the human status text for the deployment status message, e.g.
// "connections: erp-api ok".
func (p *Prepared) Summary() string {
	if p == nil || len(p.byID) == 0 {
		return ""
	}
	ids := make([]string, 0, len(p.byID))
	for id := range p.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + " " + p.byID[id].tested
	}
	return "connections: " + strings.Join(parts, ", ")
}

// reference is one connector step's connection reference.
type reference struct{ flow, step, connector, connection string }

func referencesOf(flows []*flow.Flow) ([]reference, error) {
	var refs []reference
	for _, f := range flows {
		for _, s := range f.Steps {
			if s.Kind != flow.KindConnector {
				continue
			}
			c, err := s.TypedConfig()
			if err != nil {
				return nil, fmt.Errorf("flow %s step %s: %w", f.ID, s.ID, err)
			}
			cc, ok := c.(flow.ConnectorConfig)
			if !ok {
				return nil, fmt.Errorf("flow %s step %s: unexpected config %T", f.ID, s.ID, c)
			}
			refs = append(refs, reference{f.ID, s.ID, cc.Connector, cc.Connection})
		}
	}
	return refs, nil
}

// Prepare validates every connector step of flows against defs, resolves secrets, creates the
// instances and Tests them (per TestMode). Nothing is installed until Commit. On error every
// instance created so far is closed and the error is safe to report (no secret values).
func (m *Manager) Prepare(ctx context.Context, deployment string, flows []*flow.Flow, defs []*connections.Definition) (_ *Prepared, err error) {
	byDef := map[string]*connections.Definition{}
	byConnector := map[string][]string{}
	for _, d := range defs {
		if _, dup := byDef[d.ID]; dup {
			return nil, fmt.Errorf("connection %q is defined more than once in the bundle", d.ID)
		}
		byDef[d.ID] = d
		byConnector[d.Connector] = append(byConnector[d.Connector], d.ID)
	}
	refs, err := referencesOf(flows)
	if err != nil {
		return nil, err
	}
	p := &Prepared{deployment: deployment, byID: map[string]*instance{}, defaults: map[string]string{}}
	for _, f := range flows {
		p.flowIDs = append(p.flowIDs, f.ID)
	}
	needed := map[string]bool{}
	for _, r := range refs {
		id := r.connection
		if id == "" {
			ids := byConnector[r.connector]
			switch len(ids) {
			case 0:
				return nil, fmt.Errorf("flow %s step %s: connector %q needs a connection, but the bundle defines none for it", r.flow, r.step, r.connector)
			case 1:
				id = ids[0]
				p.defaults[r.connector] = id
			default:
				sort.Strings(ids)
				return nil, fmt.Errorf("flow %s step %s: connector %q has several connections (%s); name one in the step's \"connection\"", r.flow, r.step, r.connector, strings.Join(ids, ", "))
			}
		}
		d, ok := byDef[id]
		if !ok {
			return nil, fmt.Errorf("flow %s step %s references connection %q, which is not defined in the bundle", r.flow, r.step, id)
		}
		if d.Connector != r.connector {
			return nil, fmt.Errorf("flow %s step %s: connection %q is for connector %q, not %q", r.flow, r.step, id, d.Connector, r.connector)
		}
		needed[id] = true
	}
	defer func() {
		if err != nil {
			closeAll(p)
		}
	}()
	ids := make([]string, 0, len(needed))
	for id := range needed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		inst, err := m.build(ctx, byDef[id])
		if err != nil {
			return nil, err
		}
		p.byID[id] = inst
	}
	return p, nil
}

func (m *Manager) build(ctx context.Context, d *connections.Definition) (*instance, error) {
	conn, err := m.Registry.New(d.Connector)
	if err != nil {
		return nil, fmt.Errorf("connection %q: %w", d.ID, err)
	}
	inst := &instance{id: d.ID, connectorID: d.Connector, def: d, conn: conn}
	fail := func(err error) (*instance, error) {
		closeConn(conn)
		return nil, m.Redactor.RedactError(err)
	}
	desc, err := conn.Describe(ctx)
	if err != nil {
		return fail(fmt.Errorf("connection %q: describe %s: %w", d.ID, d.Connector, err))
	}
	inst.secretPaths = connections.SecretPaths(desc.GetConnectionSchema())
	res, err := m.Resolver.Resolve(ctx, d, inst.secretPaths)
	if err != nil {
		return fail(err)
	}
	inst.cfg = res.Config
	inst.tested = "untested"
	if m.TestMode == TestOff {
		return inst, nil
	}
	tctx, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()
	ok, diag, terr := safeTest(tctx, conn, res.Config)
	diag = m.Redactor.Redact(diag)
	switch {
	case errors.Is(terr, connector.ErrNotImplemented):
		inst.tested = "not testable"
	case terr != nil || !ok:
		if terr != nil {
			diag = m.Redactor.Redact(terr.Error())
		}
		if diag == "" {
			diag = "connection test failed"
		}
		if m.TestMode == TestWarn {
			inst.tested = "warning: " + diag
			break
		}
		return fail(fmt.Errorf("connection %q (%s): test failed: %s", d.ID, d.Connector, diag))
	default:
		inst.tested = "ok"
	}
	return inst, nil
}

func (m *Manager) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return DefaultTestTimeout
}

func safeTest(ctx context.Context, c connector.Connector, cfg *connectorv1.ConnectionConfig) (ok bool, diag string, err error) {
	defer func() {
		if r := recover(); r != nil {
			ok, diag, err = false, "", fmt.Errorf("connector panicked during test: %v", r)
		}
	}()
	resp, err := c.Test(ctx, &connectorv1.TestRequest{Connection: cfg})
	if err != nil {
		return false, "", err
	}
	return resp.GetOk(), resp.GetDiagnostic(), nil
}

// Commit installs p for its deployment, replacing (and closing) the previous instances.
func (m *Manager) Commit(p *Prepared) {
	m.mu.Lock()
	old := m.deploys[p.deployment]
	if old != nil {
		for _, f := range old.flowIDs {
			delete(m.flows, f)
		}
	}
	m.deploys[p.deployment] = p
	for _, f := range p.flowIDs {
		m.flows[f] = p.deployment
	}
	m.mu.Unlock()
	if old != nil {
		closeAll(old)
	}
}

// Discard closes a prepared set that will not be committed.
func (m *Manager) Discard(p *Prepared) {
	if p != nil {
		closeAll(p)
	}
}

// Remove closes and forgets a deployment's instances.
func (m *Manager) Remove(deployment string) {
	m.mu.Lock()
	old := m.deploys[deployment]
	delete(m.deploys, deployment)
	if old != nil {
		for _, f := range old.flowIDs {
			delete(m.flows, f)
		}
	}
	m.mu.Unlock()
	if old != nil {
		closeAll(old)
	}
}

// Close closes every instance (runtime shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	all := m.deploys
	m.deploys = map[string]*Prepared{}
	m.flows = map[string]string{}
	m.mu.Unlock()
	for _, p := range all {
		closeAll(p)
	}
}

// Lookup implements engine.ConnectorLookup: the instance for a step of flowID.
func (m *Manager) Lookup(flowID string, cfg flow.ConnectorConfig) (engine.ConnectorEntry, error) {
	m.mu.Lock()
	p := m.deploys[m.flows[flowID]]
	m.mu.Unlock()
	if p == nil {
		return engine.ConnectorEntry{}, fmt.Errorf("flow %q has no deployed connections", flowID)
	}
	id := cfg.Connection
	if id == "" {
		id = p.defaults[cfg.Connector]
	}
	inst, ok := p.byID[id]
	if !ok || inst.connectorID != cfg.Connector {
		return engine.ConnectorEntry{}, fmt.Errorf("connector %q (connection %q) is not available to flow %q", cfg.Connector, cfg.Connection, flowID)
	}
	return engine.ConnectorEntry{Connector: &redacting{c: inst.conn, r: m.Redactor}, Connection: inst.cfg}, nil
}

// Info is a secret-free description of a live connection.
type Info struct {
	ID        string         `json:"id"`
	Connector string         `json:"connector"`
	Settings  map[string]any `json:"settings,omitempty"` // secret:// references stay references; secret-marked literals are masked
	Status    string         `json:"status"`
}

// Describe returns the live connections of a deployment without any secret value.
func (m *Manager) Describe(deployment string) []Info {
	m.mu.Lock()
	p := m.deploys[deployment]
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	var out []Info
	for _, inst := range p.byID {
		settings, _ := mask(inst.def.Settings, "", inst.secretPaths).(map[string]any)
		out = append(out, Info{ID: inst.id, Connector: inst.connectorID, Settings: settings, Status: inst.tested})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func mask(v any, path string, secretPaths map[string]bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			out[k] = mask(c, p, secretPaths)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = mask(c, path, secretPaths)
		}
		return out
	case string:
		if secretPaths[path] && !strings.HasPrefix(x, secrets.Ref) {
			return "***"
		}
	}
	return v
}

func closeAll(p *Prepared) {
	for _, inst := range p.byID {
		closeConn(inst.conn)
	}
}

func closeConn(c connector.Connector) {
	switch x := c.(type) {
	case interface{ Close() error }:
		_ = x.Close()
	case interface{ Close() }:
		x.Close()
	}
}
