<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Queues and dead letters

Status: implemented (written as built). Date: 2026-11-21.

## TLDR

`queue.QueueBackend` is the one interface between the engine and queues: ensure, publish, push
subscribe with explicit `Ack`/`Nak`/`Term`, stats, and a dead letter queue (DLQ) with list and
replay. Two backends exist: embedded NATS JetStream (`natsjs`, durable, DLQ also on server-side
exhaustion via an advisory) and an in-memory test backend. The shipped runtime wires the
**in-memory** backend, and nothing in the runtime publishes to a queue, so queue-started flows
cannot receive messages on a deployed runtime yet.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/24 (squash commit `2903374`); runtime wiring in https://github.com/nurtuo/nurtuo/pull/25 (`ee4d733`) |
| ADRs | [ADR 0007](../../adr/0007-queue-dlq-via-advisory.md), [ADR 0008](../../adr/0008-listener-source-abstraction.md) (queue listener) |
| Issues | — |
| Code | `core/runtime/internal/queue/queue.go`, `queue/natsjs/` (`natsjs.go`, `advisory.go`), `queue/memory/`, `queue/queuetest/suite.go` (conformance suite) |
| User docs | [concepts.md](../../concepts.md), [operations.md](../../operations.md) |

## Problem and goals

Flows need asynchronous hand-off with retries, ordering options and a place where poison messages
end up for inspection and replay, without a database round trip per message.

- Goals: push delivery (no polling); explicit settlement; bounded redelivery with backoff; a DLQ
  that also catches messages whose handler crashed or hung; ordering `none`, `per-key`, `strict`;
  per-queue concurrency; pause and resume; a conformance suite every backend must pass.
- Non-goals (as built): Kafka and Postgres backends (named in `QueueDef.Backend` and the package
  comment, not implemented); publishing from flows (draft [queue-step-runner](../2026-11-22-queue-step-runner.md));
  operator API for DLQ list and replay; exactly-once processing.

## Design (as built)

**Interface** (`queue.go`). `QueueDef{Name, Backend, Retention (workqueue|limits|interest), MaxAge,
MaxBytes, MaxDeliveries (default 5), Backoff []Duration, DeadLetter (informational), Ordering,
Concurrency}`. `Message{ID, Key, Properties, Data}`. `Delivery{Message, Attempt, Ack, Nak(delay),
Term}`. `QueueBackend{Ensure, Remove, Publish, Subscribe, Stats, ListDeadLetters,
ReplayDeadLetter, Close}`. `QueueStats{Depth, OldestMessageAge, Redeliveries, DeadLetterDepth}`.
`DeadLetter{ID, Message, Reason, Attempts, At}`.

**natsjs** (`natsjs.New(Options{StoreDir, AckWait})`). Starts an embedded nats-server with
`DontListen: true` (no network port) and JetStream; empty `StoreDir` uses a temp dir removed on
`Close`. Per queue two streams: `Q_<name>` on subject `nq.<name>` and `Q_<name>_DLQ` on
`nqdlq.<name>` (ADR 0007). Retention is immutable after creation; other limits are updated by
`Ensure`. `Publish` sets `Nats-Msg-Id` from `Message.ID`, so a non-empty id de-duplicates within
the stream's window. `Subscribe` creates or re-attaches a durable push consumer with
`AckWait` (default 30 s), `MaxDeliver` and `MaxAckPending = concurrency`; strict ordering forces
concurrency 1; per-key ordering serialises handlers per `Key` inside the process. `Nak` uses the
given delay or the `Backoff` entry for the attempt; on the last attempt it dead-letters. `Term`
dead-letters with reason "terminated". Dead-lettering copies the message with reason and attempt
headers to the DLQ and terminates it in the source. `onMaxDeliveries` subscribes to
`$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>` and moves messages the server gave up on
(ack-wait exhaustion) to the DLQ. `Pause` detaches from the durable consumer, `Resume` re-attaches.

**memory** (`memory.New()`). In-process work-queue semantics only; retention limits are not
enforced; nothing survives a restart. Passes the same conformance suite.

**Listener integration.** `listener.QueueSource` subscribes with durable `nurtuo-<queue>`, turns
each message into one document (id from `Message.ID` or `q-<n>`, `queue.key` prop), acks when the
flow succeeds and naks with delay 0 when the handler returns an error or any document failed
(see [listeners](2026-11-21-listeners.md)).

**Runtime wiring.** `RunWith` creates `NewEngineDeployer(eng, memory.New(), src)` with the comment
"In-memory queues until the embedded NATS backend is wired into the runtime process".
`EngineDeployer.Apply` calls `Queues.Ensure(ctx, queue.QueueDef{Name: sc.Queue})` for each flow
with a queue start, so every queue gets default settings. No production code calls `Publish`,
`ListDeadLetters`, `ReplayDeadLetter` or `Stats`; the supervisor commands are only
`pause_listener` and `resume_listener`.

## Contracts

| Contract | Stability |
|---|---|
| `QueueBackend` Go interface and types | internal (all under `internal/`) |
| JetStream naming `Q_<name>`, `Q_<name>_DLQ`, subjects `nq.<name>`, `nqdlq.<name>`, durable `nurtuo-<queue>` | internal, but persisted in `StoreDir`: renaming breaks existing stores |
| DLQ headers (reason, attempts, original id/key/properties as `Nurtuo-*`) | internal |
| Document prop `queue.key` on queue-started flows | experimental (visible to flow authors) |
| Queue component definition in bundles | does not exist yet (queues are implied by `start.queue`) |

## Security

- The embedded server has no network listener (`DontListen`), so queues are reachable only from
  inside the runtime process.
- Message payloads are stored unencrypted in `StoreDir` (natsjs) or memory.
- No size limit per message on `Publish`; a queue-started flow spools payloads like any other.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `queuetest.Run` cases `publishSubscribeAck`, `unknownQueue`, `nakRedelivers`, `maxDeliveriesDLQ`, `termAndReplay`, `strictOrdering`, `perKeyOrdering`, `concurrencyLimit`, `pauseResume`, `statsDepth`, `removeQueue` (`queue/queuetest/suite.go`) | backend conformance | `cd core/runtime && GOWORK=off go test -race ./internal/queue/...` | run by `TestConformance` in both `memory` and `natsjs` |
| `TestRetentionModes` (`queue/natsjs/natsjs_test.go`) | workqueue, limits, interest | same | |
| `TestMaxDeliveriesAdvisoryDeadLetters` | ack-wait exhaustion lands in the DLQ through the advisory | same | uses a short `AckWait` |
| `TestFromStartQueueAckNack` (`listener/factory_test.go`) | queue listener acks on success, naks on failure | `... ./internal/listener/...` | memory backend |
| `TestPerfBudget` scenario `queue_pickup_natsjs` (`internal/bench`) | publish to handler latency on natsjs | see [performance-budget](2026-11-21-performance-budget.md) | `NURTUO_PERF=1` only |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The runtime uses `memory.New()`. Every queued message is lost when the runtime restarts or is redeployed, and dead letters vanish with it; the durable JetStream backend, its DLQ advisory and retention are exercised only in tests. | critical | runtime wiring | wire `natsjs.New` with a `StoreDir` from `runtime.yaml` (for example `queue_dir`); fall back to memory only with an explicit setting |
| 2 | Nothing publishes to queues on a deployed runtime: no `queue` step runner, no gateway command, no HTTP endpoint. A flow with `start.type: queue` deploys, its listener runs, and it never receives a message. | high | engine, runtime | draft [queue-step-runner](../2026-11-22-queue-step-runner.md) (publish step and operator publish command) |
| 3 | DLQ list and replay exist in the backend but are not exposed (no supervisor command, no control plane API). An operator cannot see or replay a dead-lettered order. | high | runtime, control plane | add `list_dead_letters` / `replay_dead_letter` commands and a control plane endpoint, covered by the queue-step-runner draft |
| 4 | Queues get only default settings (`QueueDef{Name}`): no way to set `MaxDeliveries`, `Backoff`, `Ordering`, `Concurrency` or retention from a bundle. `QueueSource` naks with delay 0, so a failing message is retried 5 times back to back within milliseconds, then dead-lettered; a downstream outage of a few seconds dead-letters everything. | high | runtime, bundle format | a `queue` component kind in bundles with these fields; default backoff (for example 1 s, 5 s, 30 s, 2 min) |
| 5 | A dead-letter copy that fails to publish (DLQ stream full under `MaxBytes`) leaves the message in place and the advisory is not retried; the message is visible only in `Stats` depth. | medium | natsjs | retry with backoff and log; alert on DLQ publish failures |
| 6 | Per-key ordering is enforced in-process only. Two runtimes consuming the same durable (future HA) would process the same key concurrently. | medium | natsjs | partitioned subjects or a single consumer per key range when distributed roles exist |
| 7 | `Remove` on undeploy deletes the stream and its DLQ with any unprocessed messages; there is no drain. | medium | natsjs | drain or keep-on-undeploy option |
| 8 | `QueueDef.Backend` accepts `kafka` and `postgres` in comments and docs but no such backend exists. | low | docs | remove from docs until implemented |

## Improvement opportunities

- Queue stats and DLQ depth in deployment status for the console.
- Coalescing duplicate messages by key (Open Mercato `queue-job-coalescing`).

## References

- Open Mercato: `queue-job-coalescing`, `queue-status-dashboard`, `data-sync-retry-resume-actions`.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
