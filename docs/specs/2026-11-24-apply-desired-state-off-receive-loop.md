<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Run ApplyDesiredState off the gateway receive loop

Status: approved. Date: 2026-11-24.

## TLDR

Implementation detail of gap 1 in the accepted
[runtime gateway protocol v1](implemented/2026-11-21-runtime-gateway-protocol-v1.md). The runtime
client applies `DesiredState` in a worker goroutine with a "latest desired state wins" slot, so the
receive loop keeps reading and executing commands during a slow apply. No wire, proto, API or CLI
change.

## Status and links

| | |
|---|---|
| Issues | https://github.com/nurtuo/nurtuo/issues/60 |
| Code | `core/runtime/internal/gateway/`, `core/runtime/internal/supervisor/` |

## Problem and goals

With `connection_test: strict`, one apply can take 15 s per connection. It ran on the receive
goroutine, so a `pause_listener` sent meanwhile was not read, and the control plane's `sendCommand`
timed out after 10 s.

- Goals: commands are read and answered while an apply runs; applies never queue as a backlog.
- Non-goals: any change to message semantics, command timeout, when statuses are sent, or the
  `Handler` interface. An in-flight apply is not cancelled by a newer desired state; it finishes
  (or is cancelled only when the session ends).

## Design

- `gateway.session` starts one apply worker per session and a single-entry slot holding the newest
  `DesiredState`. A receive of `DesiredState` stores it in the slot (replacing a waiting one) and
  wakes the worker without blocking. The worker takes the slot, calls `Handler.ApplyDesiredState`,
  logs errors as before, and loops.
- Commands stay on the receive loop (`HandleCommand`, result enqueued as before).
- When the session ends, its context is cancelled and `session` waits for the worker before
  returning, so applies of two sessions never overlap and the old behaviour (no apply outliving the
  session) holds.
- `Supervisor.Reconcile` held `mu` for the whole apply, which would still block
  `ReportChanges` (called by `HandleCommand`) behind a slow apply. Add `applyMu` to serialise
  reconciles and keep `mu` for short critical sections on `revision` and `last` only. `Revision()`
  and `ReportChanges` no longer wait for an apply.

## Contracts

None changed. `gateway.Handler` is internal and unchanged.

## Security

No change.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestCommandHandledDuringSlowApply` in `core/runtime/internal/gateway/client_test.go` | `pause_listener`-style command result arrives while the deployer is blocked in `Apply` | `cd core/runtime && GOWORK=off go test -race ./internal/gateway/...` | uses a blocking fake deployer and the real `supervisor.Handler` |
| `TestLatestDesiredStateWins` in the same file | while apply of revision 1 is blocked, revisions 2 and 3 are sent; only 1 and 3 are applied | same | |
| `TestReportChangesDuringReconcile` in `core/runtime/internal/supervisor/reconcile_test.go` | `ReportChanges` and `Revision` return while a reconcile is blocked | `cd core/runtime && GOWORK=off go test -race ./internal/supervisor/...` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A status reported by a command while an apply runs carries the last fully applied revision | low | runtime | acceptable; the apply reports its own statuses with the new revision |
