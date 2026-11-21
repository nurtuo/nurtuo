# Performance harness

Targets (README): platform overhead p99 < 10 ms, queue pickup p99 < 5 ms.

Harness: `core/runtime/internal/bench` (AGPL). Components:

- `Histogram`: HDR-style log-linear latency histogram (p50/p95/p99/max, <1% error).
- `RunEngineOverhead`: Start -> SetProperties -> Return through `engine.Engine`, one document per run,
  configurable N / concurrency / rate. With `rate > 0` latency is measured from the intended start
  time (open loop, avoids coordinated omission).
- `RunQueuePickup`: publish -> consumer-handler latency via the local `PickupBench` interface.
  `QueuePickup` (`pickup_queue.go`) adapts any `queue.QueueBackend`; `TestPerfBudget` gates on the
  real embedded JetStream backend (`queue/natsjs`, scenario `queue_pickup_natsjs`) and also records
  the in-memory channel implementation as an informational scheduler floor
  (`queue_pickup_memory_floor`).
- `TestPerfBudget`: measures every scenario `-perf.repeats` times (default 3) and fails only when the
  **median** of the p99 values exceeds a budget (or when any run reports errors), so one noisy run
  does not fail it. It writes JSON results. It is skipped unless `NURTUO_PERF=1` is set, because a
  latency measurement is meaningless while other test packages (or `-race`) compete for the CPU.

## Measured results (queue pickup on natsjs)

Run on a Windows 11 developer laptop (not a fixed perf runner), `go1.27.4 windows/amd64`, no `-race`,
defaults: n=5000, concurrency=8, open loop 2000 ops/s, 3 consecutive runs of
`NURTUO_PERF=1 go test ./internal/bench -run TestPerfBudget -count=1 -v` (before the median gate existed). Budget: queue pickup p99 < 5 ms,
engine overhead p99 < 10 ms.

| Run | engine overhead p99 | pickup (natsjs) p50 / p95 / p99 | pickup max |
|-----|---------------------|---------------------------------|------------|
| 1   | 0.782 ms            | 0.000 / 0.000 / 0.000 ms        | 1.115 ms   |
| 2   | 0.786 ms            | 0.000 / 0.000 / 0.000 ms        | 13.053 ms  |
| 3   | 0.705 ms            | 0.000 / 0.000 / 0.000 ms        | 0.562 ms   |

All runs passed both budgets (no errors). **Caveat, read before quoting these numbers:** the
pickup percentiles of `0.000 ms` do not mean zero latency. Go's monotonic clock on Windows has a
tick of roughly 0.5-1 ms, so a publish -> handler delta shorter than one tick reads as 0. The
honest conclusion is "p99 is below the clock resolution (about 1 ms) and well under the 5 ms
budget", not an exact sub-microsecond figure. One run saw a single 13 ms outlier that did not
reach p99. The same measurement was taken for the in-memory floor and also reads 0. A precise
figure needs a Linux run on the fixed perf runner (Linux nanotime is ns resolution); that run has
not been done.

## Running locally

```sh
cd core/runtime
NURTUO_PERF=1 go test -p 1 ./internal/bench -run TestPerfBudget -count=1 -v \
  -perf.out=perf.json -perf.n=5000 -perf.concurrency=8 -perf.rate=2000 -perf.repeats=3 \
  -perf.overhead-budget=10ms -perf.pickup-budget=5ms
```

Flags: `-perf.out`, `-perf.n`, `-perf.concurrency`, `-perf.rate` (0 = closed loop),
`-perf.repeats`, `-perf.overhead-budget`, `-perf.pickup-budget`. Use `-p 1` so no other package's
tests run in parallel, and do not add `-race`. Results are machine dependent; compare on the same
fixed runner only.

## CI (nightly + `perf` label)

The regular `go` CI job runs `go test -race ./...`, where `TestPerfBudget` is skipped. It runs in its own
`perf` step of `.github/workflows/ci.yml` (shared runner, `NURTUO_PERF=1`, `-p 1`, median of 3, no `-race`).
Shared runners are noisy, so the median gate catches real regressions only; for trend tracking use a fixed
runner. Snippet for a nightly `bench` job on such a runner (not yet added to `.github/workflows`):

```yaml
bench:
  if: github.event_name == 'schedule' || contains(github.event.pull_request.labels.*.name, 'perf')
  runs-on: [self-hosted, perf]   # fixed runner; shared runners are too noisy for p99
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with: { go-version-file: core/runtime/go.mod }
    - name: Run perf harness
      working-directory: core/runtime
      env: { NURTUO_PERF: "1" }
      run: >
        go test -p 1 ./internal/bench -run TestPerfBudget -count=1 -v
        -perf.out=${{ github.workspace }}/perf.json
    - uses: actions/upload-artifact@v4
      if: always()
      with: { name: perf-results, path: perf.json }
```

The job fails when the median p99 of the repeated measurements exceeds a budget. The ">10 % regression vs published baseline" check from
ARCHITECTURE.md should compare `perf.json` with the previous nightly artifact (follow-up).
