<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Performance budget and harness

Status: implemented (written as built). Date: 2026-11-21.

## TLDR

The README promises platform overhead p99 < 10 ms and queue pickup p99 < 5 ms.
`core/runtime/internal/bench` measures both with an HDR-style histogram and open-loop load, and
`TestPerfBudget` fails when the median p99 of three runs exceeds a budget. It runs only with
`NURTUO_PERF=1`. The "overhead" scenario covers the bare interpreter (start, setProperties,
return), not HTTP, connectors or spooling, and the published pickup numbers are below the Windows
clock resolution, so neither target is demonstrated for a real flow yet.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/24 (squash commit `2903374`) |
| ADRs | — |
| Issues | — |
| Code | `core/runtime/internal/bench/` (`hist.go`, `load.go`, `overhead.go`, `pickup.go`, `pickup_queue.go`, `perf_test.go`); placeholders in `core/runtime/test/bench/bench_test.go`; CI job `perf` in `.github/workflows/ci.yml` |
| User docs | [perf.md](../../perf.md) |

## Problem and goals

Low latency is a product claim. It needs a repeatable measurement, a gate that catches
regressions, and honest numbers.

- Goals: p50/p95/p99/max with < 1 % error; open-loop load to avoid coordinated omission; a gate
  that tolerates one noisy run; machine-readable results.
- Non-goals (as built): end-to-end latency through HTTP and connectors; throughput and memory
  budgets; trend tracking against a baseline.

## Design (as built)

- `Histogram` (`hist.go`): log-linear buckets in nanoseconds, `Record`, `Quantile`, `Summary`
  (`p50_ms`, `p95_ms`, `p99_ms`, `max_ms`).
- `LoadConfig` and `drive` (`load.go`): N operations at a concurrency; with `Rate > 0` each
  operation's latency is measured from its intended start time (open loop).
- `RunEngineOverhead` (`overhead.go`): flow start, setProperties, returnDocuments on
  `engine.New()` with one document per run, 50 warm-up runs, no I/O.
- `RunQueuePickup` (`pickup.go`) over the `PickupBench` interface; `QueuePickup` (`pickup_queue.go`)
  adapts any `queue.QueueBackend` (ensure a workqueue queue, subscribe, publish, record publish to
  handler time).
- `TestPerfBudget` (`perf_test.go`): skipped unless `NURTUO_PERF=1`; runs scenarios
  `engine_overhead` (gated), `queue_pickup_natsjs` (gated) and `queue_pickup_memory_floor`
  (informational) `-perf.repeats` times (default 3); fails when the **median** p99 exceeds
  `-perf.overhead-budget` (10 ms) or `-perf.pickup-budget` (5 ms), or when a run reports errors;
  writes JSON to `-perf.out`. Other flags: `-perf.n`, `-perf.concurrency`, `-perf.rate`.
- CI: job `perf` runs `go test -p 1 -count=1 -v -run TestPerfBudget ./internal/bench` with
  `NURTUO_PERF=1` on a shared GitHub runner (no `-race`). The nightly job on a fixed runner in
  [perf.md](../../perf.md#ci-nightly--perf-label) is a snippet, not added to the workflow.
- `core/runtime/test/bench/bench_test.go` holds two benchmarks that call
  `b.Skip("TODO(M1): ...")` (JetStream publish to push handler, queue to Map to mock HTTP).

## Contracts

| Contract | Stability |
|---|---|
| Budgets p99 engine overhead 10 ms, queue pickup 5 ms | product claim (README); gate values are flags |
| `NURTUO_PERF=1` and `-perf.*` flags | internal (CI and developers) |
| `perf.json` report (`generated_at`, `budgets_ms`, `results`, `pass`) | internal |

## Security

Not applicable; the harness runs in-process with no network listener (embedded NATS uses
`DontListen`).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestPerfBudget` (`internal/bench/perf_test.go`) | the two budgets | `cd core/runtime && NURTUO_PERF=1 go test -p 1 ./internal/bench -run TestPerfBudget -count=1 -v` | env-gated; skipped in the regular `go test -race ./...` |
| `BenchmarkSpool*`, `BenchmarkApplyOverhead` (`engine/docs/bench_test.go`), `BenchmarkCompile`, `BenchmarkRun*`, `BenchmarkTimeoutOverhead` (`engine/script/wasm/bench_test.go`), `BenchmarkScriptStep` | component micro-benchmarks | `go test -run x -bench . ./internal/engine/...` | not gated |
| two benchmarks in `core/runtime/test/bench/bench_test.go` | end-to-end placeholders | — | always skipped (`TODO(M1)`) |

Measured once on a Windows 11 laptop (see [perf.md](../../perf.md#measured-results-queue-pickup-on-natsjs)):
engine overhead p99 0.705 to 0.786 ms; natsjs pickup p99 reads 0.000 ms because Go's monotonic
clock on Windows ticks about every 0.5 to 1 ms. No Linux run on a fixed runner has been done.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | "Platform overhead < 10 ms" is gated on a three-step in-memory flow. An HTTP listener, a JSON map and a connector round trip (the GetEmployee shape) are not measured; a regression there, such as per-request server setup or spooling to disk, passes the gate. | high | perf harness | implement the two `TODO(M1)` end-to-end benchmarks (HTTP in, map, mock HTTP out) and gate on them |
| 2 | Pickup numbers on Windows are below clock resolution (0.000 ms), so the 5 ms gate cannot fail for small regressions there, and the published table proves only "< about 1 ms". | medium | perf harness | run and publish on Linux on a fixed runner; record that the pickup gate is meaningful only there |
| 3 | The `perf` CI job runs on shared runners, where noisy neighbours can push p99 over 10 ms; the median of three reduces but does not remove false failures, and real regressions under the budget go unnoticed (no baseline comparison). | medium | CI | the nightly fixed-runner job from perf.md and a > 10 % regression check against the previous artifact |
| 4 | The harness gates only latency, not memory. A change that buffers whole payloads in memory instead of spilling them would pass, because the benchmark documents have no payload. | medium | perf harness | a memory scenario with `Spool.MemoryInUse` and heap high-water mark |
| 5 | The pickup gate measures `natsjs`, but the runtime ships the in-memory backend (see [queues-and-dead-letter](2026-11-21-queues-and-dead-letter.md)); the number describes code that deployed runtimes do not run. | low | perf harness, runtime | resolved when the runtime wires natsjs |
| 6 | No saturation behaviour is measured: what p99 does at 2x the rated load, and whether the runtime sheds load or queues without bound. | medium | perf harness | a step-load scenario; Open Mercato `runtime-saturation-observability` |

## Improvement opportunities

- Emit the histogram summaries as OpenTelemetry metrics once telemetry exists, so production and
  benchmark numbers use the same definitions.
- Store `perf.json` artifacts and plot p99 per commit.

## References

- Open Mercato: `runtime-saturation-observability` (saturation signals and load shedding).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
