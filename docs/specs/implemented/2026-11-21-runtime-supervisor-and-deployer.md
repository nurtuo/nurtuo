<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Runtime supervisor and deployer

Status: implemented. Date: 2026-11-21 (written as built on 2026-11-22).

## TLDR

The supervisor (`core/runtime/internal/supervisor`) turns a `DesiredState` from the gateway into
`Apply`/`Remove` calls on a `Deployer`, reports state transitions, and advances the applied
revision only when every deployment succeeded. The production deployer
(`runtime.EngineDeployer`) reads a bundle from `bundle_dir`, validates its flows, prepares
connections and starts HTTP and queue listeners. Without `bundle_dir` the runtime silently uses an
in-memory placeholder that runs nothing.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/25 (squash commit `ee4d733`) |
| ADRs | [ADR 0009](../../adr/0009-outbound-only-runtime-gateway.md), [ADR 0008](../../adr/0008-listener-source-abstraction.md) (listeners the deployer starts), [ADR 0006](../../adr/0006-bundle-format-and-domain-separated-signature.md) (signed bundles) |
| Code | `core/runtime/internal/supervisor/{deployer.go,handler.go,reconcile.go}`, `core/runtime/internal/runtime/{run.go,deployer.go,config.go}` |
| User docs | [operations.md](../../operations.md#config-file-runtimeyaml) |

## Problem and goals

The control plane says what should run; the runtime must converge on it, survive partial
failures, and report precisely what happened, without the control plane on the data path.

- Goals: level-triggered, idempotent reconcile; per-deployment failure isolation; a revision that
  only advances on full success so the control plane resends; listener pause/resume commands whose
  state survives unrelated re-applies.
- Non-goals (as built): bundle download, persistent local state across restarts, health probes of
  running flows, resource limits per deployment.

## Design (as built)

**Seams.** `supervisor.Deployer` has `Actual(ctx)`, `Apply(ctx, Deployment)` and
`Remove(ctx, id)`; `Apply` must be idempotent. `supervisor.Deployment{ID, BundleDigest, QueueIDs,
ListenerIDs}` mirrors the proto. An optional `StateProvider{State(id) (state, message)}` lets the
deployer report `paused` and a message (for example `connections: erp-api ok`).

**Reconcile** (`Supervisor.Reconcile(ctx, revision, desired, report)`), serialised by a mutex:

1. `Actual` lists running deployments.
2. Deployments not in `desired` are removed in sorted order; a failed `Remove` reports `error`.
3. Each desired deployment that is new or differs (`equal`: same digest and same queue/listener id
   sets) reports `applying`, then `Apply`; failure reports `error` with the error text; success
   reports the live state from `StateProvider` (default `running`).
4. Errors are joined; `s.revision` is updated only if there were none.

`Supervisor.ReportChanges` re-reports deployments whose live state changed (used after a command).
`supervisor.Handler` adapts the supervisor to `gateway.Handler`: it converts proto deployments,
dispatches commands from its `Commands` map (unknown name: `ok=false`, `unknown command: <name>`)
and calls `ReportChanges` after a successful command.

**Wiring** (`runtime.RunWith` in `run.go`):

- `bundle_dir` empty: `newMemDeployer()`, a map that records deployments and runs nothing.
- `bundle_dir` set: `DirBundleSource(dir)` reads `<dir>/<digest>.json`; if `trusted_keys` is set,
  `SignedBundleSource(dir, keys)` reads `<dir>/<hex>.bundle`, checks the SHA-256 against the digest,
  verifies the Ed25519 signature with `bundle.Verify` against each key and returns
  `{"flows":[...],"connections":[...]}` from the manifest components.
- The engine gets `listener.Register`, a WASM script runner with an empty module registry (script
  steps fail with "script is not registered"), and, unless the caller supplied one, a
  `connhost.Manager` as connector lookup (see the connections and secrets spec).
- Queues are `memory.New()` (in memory, not JetStream).
- `ed.Commands()` provides `pause_listener` and `resume_listener` (arg `listener` =
  `<deployment>/<flow>`); `EngineDeployer.Shutdown` stops all listeners with a 10 s timeout on exit.

**EngineDeployer.Apply** (`core/runtime/internal/runtime/deployer.go`): fetch bundle from the
source, `ParseBundle` (one flow document or `{"flows":[...],"connections":[...]}`, each flow
validated), `Conns.Prepare` (resolve secrets, create and test connector instances), for each flow
`listener.StartOf`; queue starts `Ensure` the queue; `listener.ForFlow` builds the desired
listener keyed `<deployment>/<flow>` with the bundle digest as spec fingerprint. Under the lock it
replaces the deployment's flows in the shared `flows` map, commits prepared connections and calls
`reapplyLocked`, which converges `listener.Manager` on the union of all deployments' listeners.
`Remove` drops flows, paused flags, desired listeners and connections of the deployment.
`State` reports `paused` when every listener of the deployment is paused, with the connection
summary and paused listeners in the message.

## Contracts

| Contract | Stability | Notes |
|---|---|---|
| `runtime.yaml` keys `bundle_dir`, `trusted_keys`, `http_addr`, `connection_test` | stable | `runtime.Config`; documented in operations.md |
| Bundle file layout `<bundle_dir>/<digest>.json` (unsigned) and `<bundle_dir>/<hex>.bundle` (signed) | experimental | replaced once `FetchArtifact` exists |
| Unsigned bundle content: one flow document, or `{"flows":[...],"connections":[...]}` | experimental | `ParseBundle` |
| Commands `pause_listener`, `resume_listener` with arg `listener` | stable | `EngineDeployer.Commands` |
| Listener key `<deployment>/<flow>` | stable | visible in commands and status messages |
| `supervisor.Deployer`, `StateProvider`, `gateway.Handler` | internal | Go packages under `internal/` |

## Security

- With `trusted_keys` set, only signed `.bundle` archives whose digest and signature verify are
  accepted and plain `.json` files are never read; with no keys every signed bundle is rejected.
- Without `trusted_keys` (the default) `DirBundleSource` reads `<digest>.json` without checking that
  the content hashes to the digest or is signed: anyone who can write to `bundle_dir` controls what
  runs.
- Digest strings are checked against path separators and `..` (`DirBundleSource`) or must be 64
  lowercase hex characters (`SignedBundleSource`).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestReconcileStartUpdateStop`, `TestReconcileFailureKeepsRevision` in `supervisor/reconcile_test.go` | reconcile and revision rule | `cd core/runtime && GOWORK=off go test -race ./internal/supervisor/...` | fake deployer |
| `TestReportsPausedAndMessageFromStateProvider`, `TestHandlerReportsAfterCommand` in `supervisor/state_test.go` | state provider, command reporting | same | |
| `TestEngineDeployerStartsHTTPAndQueueListeners`, `TestEngineDeployerRejectsInvalidBundle` in `runtime/deployer_test.go` | real engine and listeners | `cd core/runtime && GOWORK=off go test -race ./internal/runtime/...` | |
| `TestSignedBundleSource`, `TestConfigTrustedKeys`, `TestInvalidFlowErrorCarriesDiagnostics`, `TestPausedListenerSurvivesSiblingApply` in `runtime/signed_test.go` | signed source, config, pause persistence | same | |
| `TestLoadConfigYAMLAndJSON`, `TestLoadConfigErrors`, `TestRunGracefulShutdown`, `TestRunRejectsInvalidConfig` in `runtime/runtime_test.go` | config and lifecycle | same | |
| `TestChain`, `TestSignedBundleRejections` in `internal/e2e/chain_test.go`; `TestSignedBundleArtifactPath` in `internal/interop/interop_test.go` | end-to-end deploy paths | `cd core/runtime && GOWORK=off go test ./internal/e2e/... ./internal/interop/...` | interop is skipped without `node` or the control plane `dist` |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | `runtime.yaml` has no `bundle_dir` (it is optional, and standalone mode never sets it): `memDeployer` accepts every deployment and the supervisor reports `running`, so the UI shows a flow as running while nothing listens | high | runtime | make the placeholder report `error` ("no bundle_dir configured") or require `bundle_dir`; test that a deployment without a bundle source never reports `running` |
| 2 | Two deployments (components) contain a flow with the same `id`, for example both bundled `get-employee`: `EngineDeployer.flows` and `connhost.Manager.flows` are keyed by flow id only, so the second `Apply` overwrites the first one's flow and connection lookup, and `Remove` of either deletes the flow the other still uses | high | deployer | key flows by `<deployment>/<flow>` throughout (the engine's `MapResolver`, `connhost.Manager.flows`); add a test with two deployments sharing a flow id |
| 3 | Without `trusted_keys`, a local user or a compromised sync job writes `<bundle_dir>/<digest>.json` with arbitrary content; the runtime runs it although the content does not hash to the digest | high | security | verify the SHA-256 of unsigned bundles too; make `trusted_keys` the default for `service install`; warn at start when it is empty |
| 4 | A runtime restarts; queues are `memory.New()`, so messages accepted by a queue listener but not yet processed are lost, and the applied revision restarts at 0 | high | runtime | wire the embedded JetStream backend (`queue/natsjs`) into the runtime process; persist the applied revision; approved spec [persist runtime queues](../2026-11-24-persist-runtime-queues.md) (issue #63, not yet fixed) |
| 5 | One deployment of ten fails `Apply` (a strict connection test during a backend outage); the revision does not advance, so on every reconnect the control plane resends and the nine healthy deployments are compared again (cheap) but the failing one is retried only on reconnect or the next desired-state change, never on a timer | medium | supervisor | retry failed deployments with backoff inside the runtime and report the next attempt time |
| 6 | `Apply` of a changed deployment makes `listener.Manager` stop the old listener and then start the new one (spec fingerprint = digest); in-flight HTTP requests drain through `http.Server.Shutdown`, but requests arriving between `Stop` and `Start` are refused, so every redeploy is a short outage of that endpoint | medium | deployer, listener | start-new-then-drain-old replacement (shared listener socket); a test that fires requests during a redeploy |
| 7 | `replay_dlq`, `run_now` and `test_execute` are listed in the proto comment but answered `unknown command`; the control plane UI cannot replay a DLQ on a runtime | medium | runtime | implement or remove from the contract comment |
| 8 | Script steps always fail at run time ("script is not registered") because bundles cannot carry WASM modules; the deploy succeeds, so the error appears only when traffic arrives | medium | deployer | validate at deploy time that every script step has a module; draft spec `bundle-artifact-delivery` |

## Improvement opportunities

- Report the bundle digest and flow ids in the `running` status message so operators can confirm
  what is live without the control plane.
- A dry-run `Apply` for the `test_execute` command, reusing `Prepare` without committing.

## References

- Open Mercato: `health-monitoring` (reporting component state), `module-runtime-start-hook`
  (reconcile on start).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
