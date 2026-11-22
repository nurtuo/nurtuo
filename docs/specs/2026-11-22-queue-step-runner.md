<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Queue step runner (publish from flows) and durable queues in the runtime

Status: draft. Date: 2026-11-22.

## TLDR

The flow schema accepts `queue` steps (`{"queue": "<name>", "operation": "publish"}`), but the
engine has no runner, so every document that reaches one fails with `no_runner`. Together with the
runtime using the in-memory queue backend and nothing else publishing, queue-started flows are
unreachable on a deployed runtime. This spec adds the `queue` step runner, wires the durable
JetStream backend into the runtime, and exposes dead letters to operators.

## Status and links

| | |
|---|---|
| Pull requests | — (builds on https://github.com/nurtuo/nurtuo/pull/24 and https://github.com/nurtuo/nurtuo/pull/25) |
| ADRs | [ADR 0007](../adr/0007-queue-dlq-via-advisory.md), [ADR 0008](../adr/0008-listener-source-abstraction.md), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) |
| Issues | — |
| Code | `core/runtime/internal/engine/runner.go` (`Registry`), `core/runtime/internal/queue/`, `core/runtime/internal/runtime/run.go` and `deployer.go`, `sdk/ts/flow-schema/schema/flow.v1.schema.json` (`queueConfig`), `scripts/gen-flow-reference.mjs` (runtime status note) |
| As-built specs | [queues-and-dead-letter](implemented/2026-11-21-queues-and-dead-letter.md), [engine-interpreter](implemented/2026-11-21-engine-interpreter.md) |

## Problem and goals

Evidence on `main` (`3d3d506`):

- `NewRegistry` registers `message`, `setProperties`, `map`; the runtime adds `start`, `script`,
  `connector`. No `queue` runner: `x.e.Registry.lookup` fails and `runStep` fails every document
  with code `no_runner`. The generated reference says so (`scripts/gen-flow-reference.mjs`, note
  for `queue`; [flow-reference.md](../flow-reference.md#queue)).
- `RunWith` passes `memory.New()` to `NewEngineDeployer`; `natsjs` is used only in tests and the
  perf harness.
- No production code calls `QueueBackend.Publish`, `ListDeadLetters` or `ReplayDeadLetter`.
- Queues are created implicitly by `EngineDeployer.Apply` as `QueueDef{Name: sc.Queue}` for queue
  starts only, with default settings.

Goals:

- A `queue` step that publishes each OK document of the set to the named queue, as part of the
  flow, with clear per-document results.
- Queues that survive restarts: the runtime uses `natsjs` with a configured store directory.
- Queue settings (retries, backoff, ordering, concurrency, retention) declared in the bundle.
- Operators can list and replay dead letters and see queue stats through the control plane.

Non-goals: Kafka or Postgres backends; cross-runtime queues (distributed roles); a `consume`
operation inside a flow (consumption stays a `start` of type `queue`); exactly-once delivery.

## Design (proposed)

**Runner.** `engine.QueueRunner{Backend queue.QueueBackend, Known func(name string) bool}`
registered with `Registry.RegisterQueueRunner` in `RunWith`. Per step execution, for each OK
document in order (not concurrently, to keep publish order equal to set order):

- build `queue.Message{ID: <flow id>:<run id>:<document id>, Key: doc.Props["queue.key"],
  Properties: doc.Props, Data: payload bytes}`; the ID makes a retried run idempotent within the
  JetStream de-duplication window;
- `Publish`; on success the document passes through unchanged (so a flow can publish and then
  return), on error it fails with code `queue.publish` and the backend message;
- unknown queue (`queue.ErrNotFound`) fails the step with `queue.unknown`.

Payloads above a configurable limit (default 8 MiB, `queue.max_message_bytes` in `runtime.yaml`)
fail with `queue.too_large` instead of being read into memory.

**Queue definitions in bundles.** A new component kind `queue` in `nurtuo.bundle/v1`
(`components/queue/<name>/<revision>.json`), body:
`{"name", "maxDeliveries", "backoff": ["1s","5s","30s","2m"], "ordering", "concurrency",
"retention", "maxAge", "maxBytes"}`. `SignedBundleSource` and `ParseBundle` read it;
`EngineDeployer.Apply` calls `Ensure` with the declared definition for every queue a flow
publishes to or starts from. A referenced queue without a definition uses defaults, with backoff
`1s, 5s, 30s, 2m` instead of immediate retries. The control plane validates that every
`queue.queue` and `start.queue` name has a definition or accepts the defaults.

**Durable backend.** `runtime.yaml` gets `queue_dir` (JetStream store directory). When set,
`RunWith` uses `natsjs.New(natsjs.Options{StoreDir: queue_dir})` and closes it on shutdown; when
empty it logs a warning and uses `memory.New()` (development only). `QueueSource` naks with the
queue's backoff instead of delay 0.

**Operator commands.** New supervisor commands `queue_stats`, `list_dead_letters`
(`{queue, limit}`) and `replay_dead_letter` (`{queue, id}`), backed by the existing
`QueueBackend` methods, and matching control plane endpoints under the runtime resource
(contract in the control plane OpenAPI, owned by the control plane area).

**Docs.** `scripts/gen-flow-reference.mjs` drops the "no runner" note for `queue`;
[flow-reference.md](../flow-reference.md) is regenerated.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `queue` step semantics: publish each OK document, pass it through | new runtime behaviour, schema unchanged | stable |
| Error codes `queue.publish`, `queue.unknown`, `queue.too_large` | new | experimental |
| Message id `<flow>:<run>:<document>` | new | internal |
| Bundle component kind `queue` | new, additive to `nurtuo.bundle/v1` | experimental |
| `runtime.yaml` `queue_dir`, `queue.max_message_bytes` | new | experimental |
| Supervisor commands `queue_stats`, `list_dead_letters`, `replay_dead_letter` | new | experimental |

## Compatibility

- Flows with `queue` steps that today fail with `no_runner` start publishing after upgrade. A flow
  that relied on that failure (for example routed it into `tryCatch` on purpose) changes behaviour;
  no such flow is expected.
- Old runtimes ignore the new `queue` component kind (they read only `flow` and `connection`), so
  a new bundle still deploys there with default queue settings.
- Switching a runtime from memory to `natsjs` loses nothing (memory held nothing durable).

## Security

- Queues stay process-local (`DontListen: true`); no network surface is added.
- Message payloads are stored unencrypted in `queue_dir`; document this in
  [security.md](../security.md) and recommend disk encryption. Document props are copied into
  message properties, so props holding secrets (for example `http.request.header.authorization`
  on `main`) would be persisted: the runner drops `http.request.header.*` props by default.
- `replay_dead_letter` is an operator action and needs the control plane permission for runtime
  operations (RBAC area).

## Testing

Acceptance tests:

| Test | Expectation |
|---|---|
| `TestQueueRunnerPublishesInOrder` (`engine`) | 3 documents publish in set order and pass through unchanged |
| `TestQueueRunnerPublishFailureFailsDocument` | a backend error fails only that document with `queue.publish` |
| `TestQueueRunnerUnknownQueue` | step-level `queue.unknown` |
| `TestQueueRunnerIdempotentID` | re-running the same run id publishes once on natsjs (de-duplication) |
| `TestHTTPToQueueToFlow` (`internal/e2e`) | HTTP flow publishes to `orders`; a second flow with `start.type: queue` receives it |
| `TestQueueSurvivesRestart` (`internal/e2e`) | publish, stop the runtime before consumption, start it with the same `queue_dir`, message is delivered |
| `TestQueueDefinitionFromBundle` | `maxDeliveries: 2` and backoff from the bundle are applied (`Stats`, DLQ after 2 attempts) |
| `TestDeadLetterCommands` | `list_dead_letters` lists the parked message, `replay_dead_letter` redelivers it |
| reference example `examples/flows/reference/queue.flow.json` | still valid; generated reference has no "no runner" note |

Run: `cd core/runtime && GOWORK=off go test -race ./internal/engine/... ./internal/queue/... ./internal/e2e/...`.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Publish is not transactional with the rest of the flow: a flow publishes and then fails at a later connector step; the message is already queued and the HTTP caller gets 500 and retries, producing a duplicate. | medium | design | document at-least-once semantics; idempotent message ids per run; an outbox pattern later |
| 2 | `queue_dir` on a full disk: JetStream refuses writes, every publish fails with `queue.publish`, and HTTP flows that publish return 500. | medium | operations | disk usage in queue stats; `maxBytes` defaults per queue |
| 3 | Sequential publishing adds one round trip per document; a set of 10,000 documents takes noticeably longer than a batch publish. | low | performance | async publish with a bounded window, keeping order, after measuring |

## Improvement opportunities

- A `notify` runner can reuse the same publish path (to a notification channel), removing the
  second `no_runner` kind.
- Queue depth and DLQ depth in the console (Open Mercato `queue-status-dashboard`).

## References

- Open Mercato: `queue-job-coalescing` (de-duplicate jobs by key), `queue-status-dashboard`,
  `data-sync-retry-resume-actions` (operator retry and resume).

## Changelog

- 2026-11-22: created.
