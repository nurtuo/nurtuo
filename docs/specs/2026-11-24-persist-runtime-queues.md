<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Persist runtime queues and applied revision

Status: approved. Date: 2026-11-24.

## TLDR

Today a runtime restart loses every queued message (queues are `memory.New()`) and the applied revision restarts at 0. This spec wires the embedded JetStream backend (`queue/natsjs`) into the runtime process, stores its data on disk, and persists the applied revision. Owner decisions D1-D4 are recorded below (approved 2026-11-24); implementation may start.

## Status and links

| | |
|---|---|
| Pull requests | [PR #83](https://github.com/nurtuo/nurtuo/pull/83) (this spec); implementation PR to follow |
| ADRs | [ADR 0007](../adr/0007-queue-dlq-via-advisory.md) (queues and DLQ) |
| Issues | https://github.com/nurtuo/nurtuo/issues/63 (gap 4 of [runtime supervisor and deployer](implemented/2026-11-21-runtime-supervisor-and-deployer.md), from https://github.com/nurtuo/nurtuo/pull/47). Unblocks [distributed runtime roles](2026-11-22-distributed-runtime-roles.md) (queue section, gap 2) |
| Code | `core/runtime/internal/runtime/run.go` (`NewEngineDeployer(eng, memory.New(), src)`), `core/runtime/internal/runtime/config.go` (`Config`), `core/runtime/internal/queue/natsjs/natsjs.go` (`Options.StoreDir`), `core/runtime/internal/supervisor/reconcile.go` (`Supervisor.revision`, `Revision()`) |

## Problem and goals

Scenario: a runtime restarts (upgrade, crash, OS reboot). Messages accepted by a queue listener but not yet processed are gone, and `Hello` reports revision 0, so the control plane resends the whole desired state.

- Goals: queued messages survive a runtime restart; the applied revision survives a restart; failure to open the persistent state is reported, not hidden.
- Non-goals: HA, clustering, runtime roles, JetStream replicas, leader election (all in the distributed runtime roles spec); changing the queue interfaces (`queue.QueueBackend`); changing `memory` or `natsjs` packages beyond what wiring needs; workspace isolation.

## Decisions

Approved by Łukasz on 2026-11-24 (12:30 Europe/Warsaw); every recommendation was accepted. No blocking owner question remains.

- **D1 State directory and key.** New optional `state_dir` key in `runtime.yaml`. Standalone sets it to `<data-dir>/runtime`. `service install` sets it to the OS data root. This matches how standalone already uses `--data-dir` and `secrets/`.
- **D2 Optional key.** The key is optional. When it is unset, the runtime keeps the in-memory queue and logs a startup warning (same idea as the gap 1 `bundle_dir` warning). Existing configs keep working. Installers always set `state_dir`.
- **D3 Fail closed.** On backend failure the runtime returns an error and the service restarts. No silent fallback to the in-memory queue. Same pattern as signed bundles and secrets.
- **D4 Revision file.** The applied revision is persisted in an atomically written `<state_dir>/revision` file. A corrupt file fails start instead of silently resetting to 0.

## Design

Facts from the code: `natsjs.Options.StoreDir` already exists (empty means a temp dir removed on `Close`); `natsjs.New` runs an in-process server with `DontListen: true`, so there is no network surface. `Config` has no data or state directory key. The supervisor keeps the revision only in memory, and advances it only when every deployment applied.

1. **State directory.** The runtime gets one state directory, set by the optional `state_dir` key. Standalone sets it to `<data-dir>/runtime`; `service install` sets it to the OS data root. JetStream uses `<state_dir>/jetstream`; the revision lives in `<state_dir>/revision`. Directory mode 0700, files 0600 (as `secrets/` in standalone). The directory is created if missing. (D1)
2. **Queue backend.** In `RunWith`, when `bundle_dir` is set and `state_dir` is set, build `natsjs.New(Options{StoreDir: ...})` instead of `memory.New()` and pass it to `NewEngineDeployer`; close it after `ed.Shutdown`. Streams are durable, so unacked messages are redelivered after restart through the existing `AckWait` mechanism. When `state_dir` is unset, keep `memory.New()` and log a startup warning that queued messages and the applied revision do not survive a restart. (D2)
3. **Applied revision.** After a successful `Reconcile`, the supervisor writes the revision atomically (write temp file in the same directory, fsync, rename). On start it is loaded and returned by `Revision()` for `Hello`. A missing file means 0; a corrupt file is an error and start fails (D4). With `state_dir` unset the revision stays in memory only (D2). A crash between a successful apply and the write leaves the older revision, so the control plane resends and the idempotent reconcile reapplies it (at-least-once, cheap).
4. **No fallback.** When `state_dir` is set and the state directory or JetStream cannot be opened, `RunWith` returns an error (the OS service then exits non-zero and is restarted, see [service exit on runtime failure](2026-11-24-service-exit-on-runtime-fail.md)). It never silently uses `memory.New()`. (D3)
5. **Existing in-memory queues.** There is nothing to migrate: in-memory messages are lost at the first restart onto the new backend.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `runtime.yaml` key `state_dir` | new, optional (D1, D2) | stable once shipped |
| `Hello.revision` | now survives restart (value semantics unchanged) | stable |
| On-disk layout `<state_dir>/jetstream`, `<state_dir>/revision` | new | internal (format may change; not for operators to edit) |

No proto change. The new config key is an operator-facing contract; its name and optionality were approved as D1 and D2.

## Compatibility

Existing `runtime.yaml` files without `state_dir` keep working (D2): they stay on the in-memory queue and log a startup warning. `service install` and `standalone` always set the key. No proto or gateway change.

## Security

- The state directory holds queued payloads (may include customer data) and must be private: 0700 / 0600 on Unix. On Windows the DACL helpers from [Windows secrets DACL](2026-11-24-windows-secrets-dacl.md) apply once they are shared; until then the Windows limitation of that spec's gap list applies (#78 covers the data root).
- JetStream has no network listener (`DontListen: true`), so no new port or credential.
- Queued messages are not encrypted at rest (same as the engine's other local state). Out of scope; recorded as a risk.
- One runtime process owns one state directory. Two processes on the same directory must fail: JetStream takes a file lock; the runtime reports the error (fail closed).

## Testing

Planned tests (same PR as the code):

| Test | What it covers | How to run |
|---|---|---|
| `TestQueueMessagesSurviveRuntimeRestart` in `runtime/` | publish to a queue listener flow, stop the runtime before the flow consumes, start again on the same state dir, message is delivered | `cd core/runtime && GOWORK=off go test -race ./internal/runtime/...` |
| `TestAppliedRevisionSurvivesRestart` in `supervisor/` | revision persisted after success, loaded on a new `Supervisor`; not advanced after a failed reconcile | `cd core/runtime && GOWORK=off go test -race ./internal/supervisor/...` |
| `TestRevisionFileCorruptFailsStart` | corrupt revision file returns an error, no reset to 0 | same |
| `TestRunFailsWhenStateDirUnusable` | unwritable state dir or locked JetStream dir makes `RunWith` return an error and never fall back to memory | `./internal/runtime/...` |
| `TestLoadConfigStateDir` | `state_dir` parsing and validation; unset is allowed | `./internal/runtime/...` |
| `TestRunWithoutStateDirWarns` | unset `state_dir` keeps the in-memory queue and logs the startup warning | `./internal/runtime/...` |
| Standalone and `service install` config tests | both set `state_dir` (`<data-dir>/runtime`, OS data root) | the touched module's `go test -race` |

Fixtures use `t.TempDir()`.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Fail-closed start (D3) turns a full or read-only disk into a runtime that never starts, so HTTP flows without queues also stop | medium | runtime | clear error message naming the directory; the service restarts and the operator fixes the disk (D3 accepts this trade-off) |
| 2 | Queued payloads sit unencrypted on disk | medium | security | file permissions now; encryption at rest is a separate spec |
| 3 | Crash between apply and revision write: control plane resends the revision | low | supervisor | reconcile is idempotent; test it |
| 4 | Several runtimes on one host share a default state directory and collide | medium | runtime | standalone uses `<data-dir>/runtime`; `service install` uses one OS data root per service (D1); a second process on the same directory fails on the JetStream lock |

## Open questions

None blocking. All owner decisions are resolved (see Decisions).

## Improvement opportunities

- Revision file could carry the bundle digests applied, to skip a resend on start.
- Reuse the same state directory for pause persistence (today in memory, see `paused` in `deployer.go`).

## References

- [distributed runtime roles](2026-11-22-distributed-runtime-roles.md) (JetStream R3, `queue` role, gap 2).
- [queues and dead letter](implemented/2026-11-21-queues-and-dead-letter.md).

## Changelog

- 2026-11-24: created (draft) for issue #63.
- 2026-11-24: approved; decisions D1–D4 recorded (Łukasz accepted all recommendations).
