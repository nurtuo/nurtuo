// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/listener"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// BundleSource returns the bundle content for a digest. The content is either one flow JSON
// document or {"flows":[<flow>, ...]}. Signature verification and unpacking of signed .nurtuo
// bundles is not done here yet: the source is expected to hand over already verified content.
type BundleSource func(ctx context.Context, digest string) ([]byte, error)

// DirBundleSource reads <dir>/<digest>.json (a "sha256:" prefix on the digest is stripped).
func DirBundleSource(dir string) BundleSource {
	return func(_ context.Context, digest string) ([]byte, error) {
		name := strings.TrimPrefix(digest, "sha256:")
		if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			return nil, fmt.Errorf("deployer: invalid bundle digest %q", digest)
		}
		return os.ReadFile(filepath.Join(dir, name+".json"))
	}
}

// SignedBundleSource reads <dir>/<hex>.bundle (a "sha256:" prefix on the digest is stripped),
// checks that the archive's SHA-256 equals the digest, verifies the Ed25519 signature against
// any of keys and returns {"flows":[...]} built from the bundle's "flow" components. Plain .json
// files are never read. With no keys every bundle is rejected.
func SignedBundleSource(dir string, keys []ed25519.PublicKey) BundleSource {
	return func(_ context.Context, digest string) ([]byte, error) {
		name, err := signedBundleName(digest)
		if err != nil {
			return nil, err
		}
		return readSignedBundle(filepath.Join(dir, name+".bundle"), digest, name, keys)
	}
}

func signedBundleName(digest string) (string, error) {
	name := strings.TrimPrefix(digest, "sha256:")
	if len(name) != 64 || strings.Trim(name, "0123456789abcdef") != "" {
		return "", fmt.Errorf("deployer: invalid bundle digest %q", digest)
	}
	return name, nil
}

func readSignedBundle(path, digest, name string, keys []ed25519.PublicKey) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(data); !matchesBundleDigest(sum[:], name) {
		return nil, fmt.Errorf("deployer: bundle content does not match digest %s", digest)
	}
	var b *bundle.Bundle
	verr := errors.New("deployer: no trusted keys configured")
	for _, k := range keys {
		if b, verr = bundle.Verify(data, k); verr == nil {
			break
		}
	}
	if verr != nil {
		return nil, fmt.Errorf("deployer: bundle %s rejected: %w", digest, verr)
	}
	var flows, conns []json.RawMessage
	for _, c := range b.Manifest.Components {
		switch c.Kind {
		case "flow":
			flows = append(flows, json.RawMessage(b.Files[c.Path]))
		case connections.Kind:
			conns = append(conns, json.RawMessage(b.Files[c.Path]))
		}
	}
	if len(flows) == 0 {
		return nil, fmt.Errorf("deployer: bundle %s contains no flows", digest)
	}
	return json.Marshal(map[string]any{"flows": flows, "connections": conns})
}

func matchesBundleDigest(sum []byte, name string) bool {
	return hex.EncodeToString(sum) == name
}

// BundleContent is the decoded, validated content of a bundle.
type BundleContent struct {
	Flows       []*flow.Flow
	Connections []*connections.Definition
}

// ParseBundleFlows decodes bundle content into validated flows (connections are ignored).
func ParseBundleFlows(raw []byte) ([]*flow.Flow, error) {
	c, err := ParseBundle(raw)
	if err != nil {
		return nil, err
	}
	return c.Flows, nil
}

// ParseBundle decodes bundle content: {"flows":[...],"connections":[...]} or a single flow document.
func ParseBundle(raw []byte) (*BundleContent, error) {
	var env struct {
		Flows       []json.RawMessage `json:"flows"`
		Connections []json.RawMessage `json:"connections"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	parts := env.Flows
	if len(parts) == 0 {
		parts = []json.RawMessage{raw} // a single flow document
	}
	out := &BundleContent{}
	for _, cr := range env.Connections {
		d, err := connections.Parse(cr)
		if err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
		out.Connections = append(out.Connections, d)
	}
	for _, p := range parts {
		f, diags, err := flow.Parse(p)
		if err != nil {
			var msgs []string
			for _, d := range diags {
				if d.Severity == flow.SeverityError {
					msgs = append(msgs, d.Code+": "+d.Message)
				}
			}
			if len(msgs) > 0 {
				err = fmt.Errorf("%w: %s", err, strings.Join(msgs, "; "))
			}
			return nil, err
		}
		out.Flows = append(out.Flows, f)
	}
	return out, nil
}

// EngineDeployer is a supervisor.Deployer that resolves a deployment's bundle, registers its flows
// with the engine and starts their triggers (HTTP / queue listeners) through listener.Manager.
type EngineDeployer struct {
	Engine *engine.Engine
	Queues queue.QueueBackend
	Source BundleSource
	Deps   listener.FactoryDeps // Queues is filled from the Queues field
	// Conns owns connector instances (nil: connector steps run against whatever connector runner
	// the engine already has; bundles' connection components are then ignored).
	Conns *connhost.Manager

	mu      sync.Mutex
	mgr     *listener.Manager
	deps    map[string]supervisor.Deployment
	desired map[string][]listener.Desired
	flows   engine.MapResolver
	owned   map[string][]string // deployment -> flow ids
	paused  map[string]bool     // listener key -> paused by command; survives re-applies
	notes   map[string]string   // deployment -> status note (connection test summary)
}

// NewEngineDeployer returns a deployer; eng may be nil for a default engine with listener starts registered.
func NewEngineDeployer(eng *engine.Engine, qb queue.QueueBackend, src BundleSource) *EngineDeployer {
	if eng == nil {
		eng = engine.New()
		listener.Register(eng.Registry)
	}
	d := &EngineDeployer{
		Engine: eng, Queues: qb, Source: src, mgr: listener.NewManager(),
		deps: map[string]supervisor.Deployment{}, desired: map[string][]listener.Desired{},
		flows: engine.MapResolver{}, owned: map[string][]string{}, paused: map[string]bool{},
		notes: map[string]string{},
	}
	if eng.Resolver == nil {
		eng.Resolver = d.flows
	}
	return d
}

// Manager exposes the listener manager (status, pause/resume commands).
func (d *EngineDeployer) Manager() *listener.Manager { return d.mgr }

func (d *EngineDeployer) Actual(context.Context) (map[string]supervisor.Deployment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]supervisor.Deployment, len(d.deps))
	for k, v := range d.deps {
		out[k] = v
	}
	return out, nil
}

func (d *EngineDeployer) Apply(ctx context.Context, dp supervisor.Deployment) error {
	if d.Source == nil {
		return errors.New("deployer: no bundle source configured")
	}
	raw, err := d.Source(ctx, dp.BundleDigest)
	if err != nil {
		return fmt.Errorf("deployer: fetch bundle %s: %w", dp.BundleDigest, err)
	}
	content, err := ParseBundle(raw)
	if err != nil {
		return err
	}
	flows := content.Flows
	var prepared *connhost.Prepared
	if d.Conns != nil {
		prepared, err = d.Conns.Prepare(ctx, dp.ID, flows, content.Connections)
		if err != nil {
			return fmt.Errorf("deployer: %w", err)
		}
		defer func() {
			if prepared != nil { // not committed
				d.Conns.Discard(prepared)
			}
		}()
	}
	deps := d.Deps
	deps.Queues = d.Queues

	var want []listener.Desired
	var ids []string
	for _, f := range flows {
		sc, isListener, err := listener.StartOf(f)
		if err != nil {
			return fmt.Errorf("deployer: flow %s: %w", f.ID, err)
		}
		ids = append(ids, f.ID)
		if !isListener {
			continue
		}
		if sc.Type == listener.StartQueue && d.Queues != nil {
			if err := d.Queues.Ensure(ctx, queue.QueueDef{Name: sc.Queue}); err != nil {
				return fmt.Errorf("deployer: ensure queue %s: %w", sc.Queue, err)
			}
		}
		des, ok, err := listener.ForFlow(dp.ID+"/"+f.ID, dp.BundleDigest, d.Engine, f, deps)
		if err != nil {
			return err
		}
		if ok {
			want = append(want, des)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range d.owned[dp.ID] {
		delete(d.flows, id)
	}
	for _, f := range flows {
		d.flows[f.ID] = f
	}
	sort.Strings(ids)
	d.owned[dp.ID] = ids
	d.desired[dp.ID] = want
	d.deps[dp.ID] = dp
	if prepared != nil {
		d.Conns.Commit(prepared)
		d.notes[dp.ID] = prepared.Summary()
		prepared = nil
	}
	return d.reapplyLocked(ctx)
}

func (d *EngineDeployer) Remove(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, fid := range d.owned[id] {
		delete(d.flows, fid)
	}
	delete(d.owned, id)
	for k := range d.paused {
		if strings.HasPrefix(k, id+"/") {
			delete(d.paused, k)
		}
	}
	delete(d.desired, id)
	delete(d.deps, id)
	delete(d.notes, id)
	err := d.reapplyLocked(ctx)
	if d.Conns != nil {
		d.Conns.Remove(id)
	}
	return err
}

// State implements supervisor.StateProvider: "paused" when every listener of the deployment is
// paused by command, otherwise "running"; the message carries the connection test summary and the
// paused listeners.
func (d *EngineDeployer) State(id string) (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.deps[id]; !ok {
		return "", ""
	}
	var paused []string
	for _, x := range d.desired[id] {
		if d.paused[x.Key] {
			paused = append(paused, x.Key)
		}
	}
	sort.Strings(paused)
	state := supervisor.StateRunning
	var parts []string
	if n := d.notes[id]; n != "" {
		parts = append(parts, n)
	}
	if len(paused) > 0 {
		parts = append(parts, "paused listeners: "+strings.Join(paused, ", "))
		if len(paused) == len(d.desired[id]) {
			state = supervisor.StatePaused
		}
	}
	return state, strings.Join(parts, "; ")
}

// reapplyLocked converges the listener manager on the union of all deployments' triggers.
func (d *EngineDeployer) reapplyLocked(ctx context.Context) error {
	var all []listener.Desired
	for _, ds := range d.desired {
		for _, x := range ds {
			x.Paused = d.paused[x.Key]
			all = append(all, x)
		}
	}
	return d.mgr.Apply(ctx, all)
}

// Shutdown stops every listener.
func (d *EngineDeployer) Shutdown(ctx context.Context) error {
	err := d.mgr.Shutdown(ctx)
	if d.Conns != nil {
		d.Conns.Close()
	}
	return err
}

// SetPaused pauses or resumes the listener key ("<deployment>/<flow>"). The state survives
// re-applies of other deployments and is dropped when the deployment is removed.
func (d *EngineDeployer) SetPaused(ctx context.Context, key string, paused bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	found := false
	for _, ds := range d.desired {
		for _, x := range ds {
			found = found || x.Key == key
		}
	}
	if !found {
		return fmt.Errorf("deployer: unknown listener %q", key)
	}
	if paused {
		d.paused[key] = true
	} else {
		delete(d.paused, key)
	}
	return d.reapplyLocked(ctx)
}

// Commands returns the control-plane commands the deployer serves: pause_listener and
// resume_listener, each with the arg "listener" = "<deployment>/<flow>".
func (d *EngineDeployer) Commands() map[string]supervisor.CommandFunc {
	mk := func(paused bool) supervisor.CommandFunc {
		return func(ctx context.Context, args map[string]string) error {
			k := args["listener"]
			if k == "" {
				return errors.New("missing arg: listener")
			}
			return d.SetPaused(ctx, k, paused)
		}
	}
	return map[string]supervisor.CommandFunc{"pause_listener": mk(true), "resume_listener": mk(false)}
}
