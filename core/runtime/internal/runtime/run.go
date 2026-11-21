// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm"
	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	"github.com/nurtuo/nurtuo/core/runtime/internal/listener"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/memory"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
	"github.com/nurtuo/nurtuo/core/runtime/internal/version"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// Options tweaks Run; the zero value is production behaviour.
type Options struct {
	Logger   *slog.Logger
	Deployer supervisor.Deployer                  // default: in-memory placeholder until the engine lands
	Gateway  func(Config) (gateway.Config, error) // test seam; default builds mTLS credentials
	// Engine is used by the bundle_dir deployer instead of a default engine. If it already has a
	// connector step runner, that runner is used as-is (embedding/tests); otherwise the runtime
	// wires connections, secrets and the connector registry itself.
	Engine *engine.Engine
	// Connectors is the native connector registry; default connhost.Builtins().
	Connectors *connhost.Registry
	// Secrets overrides the provider built from cfg.Secrets (test seam).
	Secrets secrets.Provider
}

// newConnManager builds the connection manager and returns a logger that redacts every resolved
// secret value.
func newConnManager(cfg Config, o Options, log *slog.Logger) (*connhost.Manager, *slog.Logger, error) {
	prov := o.Secrets
	if prov == nil {
		st, err := secrets.Open(cfg.Secrets)
		if err != nil {
			return nil, nil, err
		}
		prov = st
	}
	reg := o.Connectors
	if reg == nil {
		reg = connhost.Builtins()
	}
	red := secrets.NewRedactor()
	mgr := connhost.NewManager(reg, &connections.Resolver{Secrets: prov, Redactor: red})
	mgr.TestMode = cfg.ConnectionTest
	return mgr, slog.New(red.Handler(log.Handler())), nil
}

// Run starts the gateway client and supervisor and blocks until ctx is cancelled (graceful stop).
// It returns nil on a clean, context-driven shutdown.
func Run(ctx context.Context, cfg Config) error { return RunWith(ctx, cfg, Options{}) }

// RunWith is Run with injectable dependencies.
func RunWith(ctx context.Context, cfg Config, o Options) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	dep := o.Deployer
	var cmds map[string]supervisor.CommandFunc
	if dep == nil {
		if cfg.BundleDir != "" {
			// Real deployer: engine + listeners. In-memory queues until the embedded NATS
			// backend is wired into the runtime process.
			src := DirBundleSource(cfg.BundleDir)
			if keys, err := cfg.trustedKeys(); err != nil {
				return err
			} else if len(keys) > 0 {
				src = SignedBundleSource(cfg.BundleDir, keys)
			}
			eng := o.Engine
			if eng == nil {
				eng = engine.New()
			}
			listener.Register(eng.Registry)
			if !eng.Registry.Has(flow.KindScript) {
				// script steps run in the wazero sandbox; modules are not yet delivered by bundles, so an
				// empty module registry makes the step fail with a clear "script is not registered" error.
				we := wasm.NewEngine(wasm.EngineOptions{})
				defer func() { _ = we.Close(context.Background()) }()
				eng.Registry.RegisterScriptRunner(engine.NewScriptRunner(we, engine.NewScriptModules()))
			}
			ed := NewEngineDeployer(eng, memory.New(), src)
			if !eng.Registry.Has(flow.KindConnector) {
				// Production wiring: bundle connections + secrets + native connector registry.
				mgr, lg, err := newConnManager(cfg, o, log)
				if err != nil {
					return err
				}
				log = lg
				eng.Registry.RegisterConnectorLookup(mgr)
				ed.Conns = mgr
			}
			ed.Deps.HTTPAddr = cfg.HTTPAddr
			cmds = ed.Commands()
			defer func() {
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = ed.Shutdown(sctx)
			}()
			dep = ed
		} else {
			dep = newMemDeployer()
		}
	}
	build := o.Gateway
	if build == nil {
		build = defaultGatewayConfig
	}
	gc, err := build(cfg)
	if err != nil {
		return err
	}
	h := &supervisor.Handler{S: supervisor.New(dep), Commands: cmds}
	gc.Handler = h
	gc.Logger = log
	log.Info("runtime starting", "runtime_id", cfg.RuntimeID, "control_plane", cfg.ControlPlaneURL)
	client := gateway.NewClient(gc)
	h.Report = client.ReportStatus // pause/resume report deployment state changes
	err = client.Run(ctx)
	log.Info("runtime stopped")
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func defaultGatewayConfig(cfg Config) (gateway.Config, error) {
	creds, err := gateway.MTLSCredentials(cfg.CertFile, cfg.KeyFile, cfg.CAFile, cfg.ServerName)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("mtls: %w", err)
	}
	return gateway.Config{Target: cfg.ControlPlaneURL, RuntimeID: cfg.RuntimeID, Version: version.Version, TLS: creds}, nil
}

// memDeployer tracks deployments in memory; the real engine replaces it.
type memDeployer struct {
	mu sync.Mutex
	m  map[string]supervisor.Deployment
}

func newMemDeployer() *memDeployer { return &memDeployer{m: map[string]supervisor.Deployment{}} }

func (d *memDeployer) Actual(context.Context) (map[string]supervisor.Deployment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]supervisor.Deployment, len(d.m))
	for k, v := range d.m {
		out[k] = v
	}
	return out, nil
}

func (d *memDeployer) Apply(_ context.Context, dp supervisor.Deployment) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[dp.ID] = dp
	return nil
}

func (d *memDeployer) Remove(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.m, id)
	return nil
}
