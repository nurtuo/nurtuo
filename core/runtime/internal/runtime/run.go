// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm"
	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	"github.com/nurtuo/nurtuo/core/runtime/internal/listener"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/memory"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/natsjs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
	"github.com/nurtuo/nurtuo/core/runtime/internal/version"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// Options tweaks Run; the zero value is production behaviour.
type Options struct {
	Logger   *slog.Logger
	Deployer supervisor.Deployer                  // default: bundle_dir deployer, or a placeholder that reports error when bundle_dir is unset
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

// newResolver builds the secret resolver shared by connections and listeners, and returns a
// logger that redacts every value it resolves.
func newResolver(cfg Config, o Options, log *slog.Logger) (*connections.Resolver, *slog.Logger, error) {
	prov := o.Secrets
	if prov == nil {
		st, err := secrets.Open(cfg.Secrets)
		if err != nil {
			return nil, nil, err
		}
		prov = st
	}
	red := secrets.NewRedactor()
	return &connections.Resolver{Secrets: prov, Redactor: red}, slog.New(red.Handler(log.Handler())), nil
}

// newConnManager builds the connection manager over res.
func newConnManager(cfg Config, o Options, res *connections.Resolver) *connhost.Manager {
	reg := o.Connectors
	if reg == nil {
		reg = connhost.Builtins()
	}
	mgr := connhost.NewManager(reg, res)
	mgr.TestMode = cfg.ConnectionTest
	return mgr
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
	stateDir := ""
	if cfg.StateDir != "" {
		d, err := prepareStateDir(cfg.StateDir)
		if err != nil {
			return err
		}
		unlock, err := lockStateDir(d)
		if err != nil {
			return err
		}
		defer unlock()
		stateDir = d
	} else {
		warnNoStateDir(log)
	}
	dep := o.Deployer
	var cmds map[string]supervisor.CommandFunc
	if dep == nil {
		if cfg.BundleDir != "" {
			// Real deployer: engine + listeners. Queues are durable (embedded JetStream under
			// state_dir) or in-memory when state_dir is unset (D2).
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
			var pq *natsjs.Backend
			var ed *EngineDeployer
			defer func() {
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if ed != nil {
					_ = ed.Shutdown(sctx)
				}
				if pq != nil {
					if err := pq.CloseContext(sctx); err != nil {
						log.Warn("queue backend shutdown", "error", err)
					}
				}
			}()
			var qb queue.QueueBackend = memory.New()
			if stateDir != "" {
				var err error
				pq, err = openPersistentQueues(stateDir)
				if err != nil {
					return err // fail closed, no memory fallback (D3)
				}
				qb = pq
			}
			ed = NewEngineDeployer(eng, qb, src)
			res, lg, err := newResolver(cfg, o, log)
			if err != nil {
				return err
			}
			log = lg
			if !eng.Registry.Has(flow.KindConnector) {
				// Production wiring: bundle connections + secrets + native connector registry.
				mgr := newConnManager(cfg, o, res)
				eng.Registry.RegisterConnectorLookup(mgr)
				ed.Conns = mgr
			}
			ed.Deps.HTTPAddr = cfg.HTTPAddr
			ed.Deps.Secrets = res // listener auth / tls secret:// references
			cmds = ed.Commands()
			dep = ed
		} else {
			log.Warn("bundle_dir is not set; deployments will be reported as error")
			dep = newUnconfiguredDeployer()
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
	sup, err := newSupervisor(dep, stateDir)
	if err != nil {
		return err
	}
	h := &supervisor.Handler{S: sup, Commands: cmds}
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

// errNoBundleDir is what a runtime without a bundle source reports for every deployment.
var errNoBundleDir = errors.New("no bundle_dir configured: the runtime cannot run deployments")

// unconfiguredDeployer is the placeholder used when no bundle_dir is set. It refuses every
// deployment, so the supervisor reports "error" and never "running" for something nothing started.
//
// TODO(#64): phase 2 of docs/specs/2026-11-23-runtime-bundle-dir.md: FetchArtifact plus a
// standalone BundleDir and local signing key replace this placeholder for `nurtuo standalone`.
type unconfiguredDeployer struct{}

func newUnconfiguredDeployer() *unconfiguredDeployer { return &unconfiguredDeployer{} }

func (*unconfiguredDeployer) Actual(context.Context) (map[string]supervisor.Deployment, error) {
	return map[string]supervisor.Deployment{}, nil
}

func (*unconfiguredDeployer) Apply(context.Context, supervisor.Deployment) error {
	return errNoBundleDir
}

func (*unconfiguredDeployer) Remove(context.Context, string) error { return nil }
