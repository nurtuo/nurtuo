<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: ExecuteLargePayload contract test with a memory bound

Status: draft. Date: 2026-11-24.

## TLDR

The contract suite skips `ExecuteLargePayload`, so a connector that buffers a whole document
passes conformance and can OOM the runtime. This spec proposes the subtest: stream one document
slightly over 4 MiB through `Execute` and fail if the Go heap grows by more than a fixed ceiling.
The v1 spec only says "a payload larger than 4 MiB" and "`ReadMemStats` delta", so the numbers and
the new `Options` field are decisions for the owner (see "Decisions").

## Status and links

| | |
|---|---|
| Pull requests | this PR (spec only) |
| ADRs | none |
| Issues | https://github.com/nurtuo/nurtuo/issues/65 |
| Code | `sdk/go/connector/contracttest/contracttest.go` (`t.Skip` at the `ExecuteLargePayload` subtest) |
| Parent spec | gap 2 in [connector contract v1](implemented/2026-11-21-connector-contract-v1.md) |

## Problem and goals

A connector can buffer a 300 MB document and the suite never notices (gap 2, high).

- Goals: implement the subtest for in-process connectors (`Options.New`), run it in the existing
  per-connector `go test` jobs, no skip for native connectors that opt in.
- Non-goals: 300 MB in CI; bridges (`Options.Dial`; the heap of another process is not visible
  to `ReadMemStats`); changing proto or the RPC shape; `CrashBehaviour` and `Listen`.

## Design (proposed)

New optional field `Options.LargePayload *LargePayloadCase`:

- `Start *connectorv1.ExecuteStart` (operation and settings the connector needs), same defaults as
  `ExecuteCase`;
- `Want connectorv1.DocumentStatus` (default `SUCCESS`).

Behaviour of the subtest:

1. Skipped when `Options.Dial` is set ("heap not observable out of process") or `LargePayload` is
   nil ("connector provided no large payload case").
2. One document of `LargePayloadSize = 16 MiB` (4x the gRPC default message limit), generated and
   sent chunk by chunk (`DefaultChunkSize`, 64 KiB) from a reused buffer, so the test itself holds
   O(chunk) memory. The payload is not materialised as one slice.
3. Measurement: `runtime.GC()` twice, `runtime.ReadMemStats` -> `before.HeapAlloc`; run the
   stream. A sampler goroutine reads `HeapAlloc` every 5 ms and keeps the maximum (plus one read
   after the last `Recv`). Peak growth = max sample - `before.HeapAlloc`. No GC is forced during the
   run, so a buffering connector's peak is visible. One warm-up `Execute` with a 64 KiB document
   runs first so lazy initialisation is not charged.
4. Assertion: peak growth <= `LargePayloadHeapCeiling = 8 MiB` (payload/2). A streaming connector
   needs O(chunk) (the gRPC transport and bufconn add well under 4 MiB); a connector that buffers
   16 MiB, plus any copy, exceeds it. Also assert the result has the wanted status.
5. If the run is flaky (GC pacing), the test retries once before failing; that is the only
   allowed tolerance.

The measure is process-wide, so the subtest must not use `t.Parallel`, and the stream is served
in the same process, hence the ceiling includes the server side and the test client.

## Contracts

- `contracttest.Options.LargePayload` and `contracttest.LargePayloadCase`: new exported API of
  `sdk/go/connector/contracttest` (Apache-2.0, experimental, additive).
- Ceiling and size are unexported constants in the suite; connector authors cannot change them.
- No change to `nurtuo.connector.v1` proto or to v1 connector obligations beyond the existing
  statement that documents are chunked.

## Compatibility

Additive. Connectors that do not set `LargePayload` still skip the subtest. Connectors that set it
and buffer more than 8 MiB start failing; that is the intent.

## Security

Input limits: this adds a test-time guard only. It does not add a runtime limit (gap 5 and the
"Input limits" note in the v1 spec stay open).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestFakeConnectorConforms` (extended) in `sdk/go/connector/contracttest/contracttest_test.go` | passes with a streaming fake | `cd sdk/go && GOWORK=off go test ./connector/...` | |
| new `TestLargePayloadRejectsBufferingConnector` | a fake that accumulates all chunks fails the subtest | same | uses a sub-`testing.T` or a recorder |
| `connectors/{http,sftp,postgres,nats}` `TestContract` | each native connector with a `LargePayload` case | `cd connectors && go test -race ./...` | connectors that cannot take a document body (e.g. one that only reads properties) leave it nil and are listed in the implementation PR |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A native connector (for example `http` building a request body in memory) buffers 16 MiB and fails the new bound | medium | connectors | fix if trivial, otherwise leave `LargePayload` nil and file an issue; do not weaken the ceiling |
| 2 | `HeapAlloc` sampling is noisy under `-race` (race detector shadow memory is not in `HeapAlloc`, but allocation rate is higher) | medium | CI | the 2x margin over streaming need and one retry; measure on CI before approving the numbers |
| 3 | Bridges are not covered | medium | bridges | out of scope; needs RSS of the child process |

## Decisions for the owner

1. Payload size and ceiling: 16 MiB and 8 MiB (recommended). Alternative: just over 4 MiB
   (e.g. 5 MiB, ceiling 2.5 MiB), cheaper but noisier because the transport buffers are the same
   order of magnitude as the ceiling.
2. Measurement: peak `HeapAlloc` by sampling, no forced GC during the run (recommended).
   Alternative: `HeapAlloc` delta after a final `runtime.GC()`, which misses a connector that
   buffers and then drops the data.
3. Opt-in via `Options.LargePayload` (recommended; a new exported API, additive). Alternative:
   synthesise the case automatically for every connector, which cannot work because each
   connector needs its own operation and settings.
4. Scope: in-process connectors only, bridges later (recommended).

## References

- [connector contract v1](implemented/2026-11-21-connector-contract-v1.md), gap 2.

## Changelog

- 2026-11-24: created (draft), refs #65.
