<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Engine interpreter (set-at-a-time DAG execution)

Status: implemented (written as built). Date: 2026-11-21.

## TLDR

`core/runtime/internal/engine` interprets a validated flow over a **set** of documents: each step
processes the whole set, every document carries its own status (`ok`, `error`, `dropped`), and a
run ends with returned documents, failed documents, an optional stop and a deterministic trace.
Step kinds plug in through a `Registry` of `StepRunner`s; in the shipped runtime `queue` and
`notify` have no runner, and conditions use the limited `BasicEvaluator`, not CEL.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/23 (squash commit `54ca076`); runtime wiring in https://github.com/nurtuo/nurtuo/pull/25 (`ee4d733`) |
| ADRs | [ADR 0005](../../adr/0005-set-at-a-time-engine-with-per-document-status.md), [ADR 0003](../../adr/0003-cycles-are-errors.md) |
| Issues | — |
| Code | `core/runtime/internal/engine/` (`interp.go`, `runner.go`, `steps.go`, `eval.go`, `cel.go`, `script.go`), `core/runtime/internal/engine/docs/` (document model, spool, pipeline) |
| User docs | [concepts.md](../../concepts.md), [flow-reference.md](../../flow-reference.md), [connector-authoring.md](../../connector-authoring.md) |

## Problem and goals

Integration flows move many documents (rows, files, messages). The engine must batch them to
connectors, isolate a bad document from the others, keep large payloads off the heap, and give
the same result and trace for the same input.

- Goals: set-at-a-time execution; per-document failure without aborting the set; deterministic
  ordering and trace; bounded memory through disk spill; pluggable step runners; nested flows with
  a depth bound.
- Non-goals: loops (ADR 0003); parallel `branch` paths; durable execution state (a run is in
  memory and restarts from the trigger); scheduling.

## Design (as built)

**Documents** (`engine/docs`). `Document{ID, Props map[string]string, payload, status, err}`.
`Spool` creates `Payload`s: in memory up to `DocThreshold` (1 MiB default) while a shared
`TotalBudget` (64 MiB) is available, otherwise streamed to a temp file. Payloads are immutable,
re-readable and reference counted (`Retain`/`Release`; the last release deletes the spill file,
also when a reader is still open on Windows). `Document.Clone` shares the payload. `docs.Apply`
runs a per-document function over the OK documents of a set with bounded concurrency
(`GOMAXPROCS` by default), keeps order, recovers panics as document errors, and on cancellation
fails unstarted documents with the context error.

**Interpreter** (`interp.go`). `Engine{Registry, Resolver, Eval, Spool, MaxDepth}`; `engine.New()`
registers `message`, `setProperties` and `map` runners and uses `BasicEvaluator`.
`Engine.Run(ctx, flow, set, process)` finds the start step and `walk`s:

- single-output steps (`start`, `connector`, `queue`, `map`, `script`, `message`, `setProperties`,
  `notify`, `processCall`) run, then `triage` routes errored documents to the innermost `tryCatch`
  frame (or the result's `Failed` set), releases dropped ones, and the OK documents follow
  `default`. A missing runner fails every document with code `no_runner`; a runner error that is
  not a `*docs.Error` fails the remaining OK documents with that message. `start` passes through
  when no runner is registered.
- `decision` evaluates `cel` per document and splits into `true` then `false`; an evaluation error
  fails that document with code `condition`.
- `route` sends each document to the first route whose condition holds, else `default`, in
  declared order.
- `branch` runs paths `1..n` **sequentially**, each over a clone of the set (shared payloads).
- `tryCatch` pushes a frame, runs `try`; documents failed inside are retried up to `retries` times
  from a backup taken at entry, then sent down `catch` with `nurtuo.error.message`,
  `nurtuo.error.step` and `nurtuo.error.code` props and status reset to OK.
- `processCall` resolves the child flow through `Resolver`, runs it with a copy of process props
  (not copied back) at `depth+1`, and continues with the child's returned and failed documents.
  Depth above `MaxDepth` (16) fails with `processCall.depth`; resolver errors with
  `processCall.resolve`.
- `stop` records `StopInfo{StepID, Status, Message}` and ends the whole run; later walks close
  their sets. `returnDocuments` adds OK documents to `Returned`.

`Result{Returned, Failed, Stop, Trace}`; the caller owns and closes both sets. `StepTrace` records
`In`, `Out`, `Failed`, `Dropped` per step execution in execution order. On context cancellation
the partial result is returned with `ctx.Err()`.

**Runners** (`runner.go`, `steps.go`, `script.go`). `StepRunner.Run(ctx, *StepContext, *docs.Set)`;
`PerDocument` lifts a per-document function. `ConnectorRunnerFor(lookup)` sends the OK set to one
connector `Execute` stream (in memory, `memStream`, payload chunks of `chunkSize`) and maps each
`DocumentResult` back: `SUCCESS` with outputs replaces the input (props inherited, output props
win), `SUCCESS` without outputs passes it through, `APPLICATION_ERROR`/`FAILURE`/no result fails
it; tracked values become `tracked.<name>` props; duplicate input ids fail
(`duplicate_document_id`); a stream error or connector panic (`safeExecute`) fails the whole set.
Operation names `GET`, `QUERY`, `CREATE`, `UPDATE`, `UPSERT`, `DELETE` map to contract operation
types, anything else is `EXECUTE` (`operationType`). Map and script runners: see
[maps-and-expressions](2026-11-21-maps-and-expressions.md) and
[wasm-script-sandbox](2026-11-21-wasm-script-sandbox.md).

**Runtime wiring** (`core/runtime/internal/runtime/run.go`, `deployer.go`). With `bundle_dir` set,
`RunWith` uses `engine.New()` (so `BasicEvaluator`), registers the listener `start` runner, a
`ScriptRunner` with an empty `ScriptModules` registry, and the connector runner backed by the
per-deployment connection manager. `EngineDeployer` keeps deployed flows in one
`engine.MapResolver` keyed by flow id for `processCall`.

## Contracts

| Contract | Stability |
|---|---|
| Document status model and `nurtuo.error.message` / `.step` / `.code` props on the catch path | stable (visible to flow authors) |
| Error codes set by the engine: `no_runner`, `condition`, `processCall.depth`, `processCall.resolve`, `duplicate_document_id`, `payload`, `map_compile`, `script.<code>` | experimental; surfaced in failed documents and HTTP error bodies |
| `tracked.<name>` props from connector results | experimental |
| Execution order (decision `true` before `false`, branch `1..n`, route declared order) | stable (snapshot tests depend on it) |
| Go API `Engine`, `Registry`, `StepRunner`, `StepContext`, `Result`, `StepTrace`, `Evaluator`, `FlowResolver` | internal (package under `internal/`) |
| Limits `DefaultMaxDepth = 16`, `DefaultDocThreshold = 1 MiB`, `DefaultTotalBudget = 64 MiB` | internal defaults, not configurable from `runtime.yaml` |

## Security

- Flows are validated (`flow.Parse`) before they reach the engine; cycles never execute.
- A connector panic is recovered (`safeExecute`) and fails the set instead of the process; a panic
  in a per-document function is recovered by `docs.Apply`.
- Spill files are written to `os.TempDir()` by default with the process's permissions; payloads may
  contain personal data and stay on disk until released. A crash leaves them behind.
- No per-run CPU or wall-clock limit exists in the engine; the HTTP listener has none by default
  either (see [listeners](2026-11-21-listeners.md)).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestDecisionSplitsSet`, `TestDecisionConditionErrorFailsDocument`, `TestBranchRunsPathsInOrder`, `TestRouteFirstMatchThenDefault`, `TestTryCatchIsolatesFailingDocuments`, `TestTryCatchRetries`, `TestUncaughtFailureCollectedInResult`, `TestMissingRunnerFailsDocuments`, `TestProcessCall`, `TestProcessCallUnknownFlowAndRecursion`, `TestStopEndsExecution`, `TestContextCancellation`, `TestMessageReplacesPayload`, `TestBasicEvaluator` (`engine/interp_test.go`) | interpreter semantics | `cd core/runtime && GOWORK=off go test -race ./internal/engine/...` | |
| `TestGetEmployeeExample` (`interp_test.go`) | the example flow with fake connector and map runners | same | `t.Skipf` if `examples/flows/get-employee.flow.json` cannot be read |
| `TestConnectorRunnerFakeConnector`, `TestConnectorRunnerUnregistered`, `TestMapRunner`, `TestCELEvaluator`, `TestGetEmployeeFlowEndToEnd` (`engine/steps_test.go`) | connector result mapping, map runner, CEL | same | `TestGetEmployeeFlowEndToEnd` imports `github.com/nurtuo/nurtuo/connectors/http`, so the engine tests need the connectors module |
| `TestRunner*` (`engine/runner_hardening_test.go`) | missing output chunks, spool failure, duplicate and shared ids, stream error after success, connector panic | same | |
| `engine/docs/*_test.go` (`TestSpoolPayload`, `TestMemoryBounded`, `TestLargePayloadStreamsAndCleansUp`, `TestApply*`, `TestReleaseWithOpenReaderNoLeak`, ...) | spool, ref counting, pipeline | same | benchmarks in `docs/bench_test.go` |
| `TestChain`, `TestRealWiringHTTPConnector` (`core/runtime/internal/e2e`) | engine as wired by the runtime | `cd core/runtime && GOWORK=off go test -race ./internal/e2e/...` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The runtime uses `BasicEvaluator`. A decision `'id' in doc.props` or `size(doc.props.name) > 0` validates, passes `TestCELEvaluator`-style tests, and on a deployed runtime fails every document with code `condition`. The GetEmployee end-to-end test sets `CELEvaluator` explicitly, so it does not catch this. | high | runtime wiring | select `&engine.CELEvaluator{}` in `RunWith` (behaviour change for missing props: CEL errors, Basic reads ""); see [maps-and-expressions](2026-11-21-maps-and-expressions.md) |
| 2 | `queue` and `notify` steps have no runner: every document reaching them fails with `no_runner`. | medium | engine | draft [queue-step-runner](../2026-11-22-queue-step-runner.md); `notify` needs its own spec |
| 3 | `EngineDeployer` keeps all deployed flows in one `MapResolver` keyed by flow id. Deployment B that contains a flow `common` replaces deployment A's `common`; removing B deletes it, and A's `processCall` then fails with `processCall.resolve`. `processCall.revision` is ignored by `MapResolver.Resolve`. | high | runtime deployer | scope the resolver per deployment and key by (id, revision) |
| 4 | No execution time limit: a connector that never answers blocks the run, and an HTTP request with it, forever (`HTTPConfig.Timeout` is never set by `FromStart`). | medium | engine, listener | per-step and per-run deadlines in the flow or runtime config |
| 5 | Within a run, `docs.Apply` and the script runner use `GOMAXPROCS` workers each, and every HTTP request or queue delivery is its own run, so concurrent runs are unbounded. 1,000 parallel requests with 1 MiB bodies hold up to the 64 MiB spool budget in memory and spill the rest to the temp disk with no disk quota. | medium | engine, listener | a runtime-wide concurrency limit and spill quota; [performance-budget](2026-11-21-performance-budget.md) |
| 6 | Runs are not durable. A runtime restart during a run loses it: an HTTP caller gets a connection reset, and a queue delivery is redelivered only if the backend is durable (the runtime uses the in-memory one, see [queues-and-dead-letter](2026-11-21-queues-and-dead-letter.md)). | medium | engine | documented limitation; durable runs are a later milestone |
| 7 | `branch` is sequential: two independent 2 s connector calls on paths 1 and 2 take 4 s. | low | engine | parallel branch as an opt-in (Open Mercato `workflows-parallel-fork-join`) |
| 8 | The engine tests import the HTTP connector (`TestGetEmployeeFlowEndToEnd`), so a change in `connectors/http` can break `go test ./internal/engine/...` and the engine package cannot be tested without the connectors module. | low | tests | move that test to `internal/e2e` and use a fake connector in the engine package |

## Improvement opportunities

- Expose `StepTrace` in deployment status or logs; it is computed for every run and then dropped.
- A typed `RunOutcome` (returned, failed, stopped, cancelled) instead of inspecting three fields.
- Configurable spool directory and budgets in `runtime.yaml`.

## References

- Open Mercato: `workflow-run-outcomes` (explicit outcome types), `workflows-parallel-fork-join`
  (parallel branches and joins), `mutation-lifecycle` (ordered side-effect phases).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
