<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: ExecuteLargePayload contract test with a memory bound

Status: approved. Date: 2026-11-24.

## Summary

The Go connector contract suite streams one 64 MiB document in 64 KiB chunks by default and fails
if sampled live heap growth exceeds 24 MiB. In-process connectors that handle documents
incrementally can opt in. SFTP is the only built-in connector that opts in.

## Status and links

| | |
|---|---|
| Spec PR | [#80](https://github.com/nurtuo/nurtuo/pull/80) |
| Implementation on main | [#110](https://github.com/nurtuo/nurtuo/pull/110) ([5a24339](https://github.com/nurtuo/nurtuo/commit/5a24339fc0976d412ebcfc97fd97bdc3ce766309)), [#111](https://github.com/nurtuo/nurtuo/pull/111) ([90fe84c](https://github.com/nurtuo/nurtuo/commit/90fe84c19a401a752f14427173956f65e5182670)), [#113](https://github.com/nurtuo/nurtuo/pull/113) ([9a0a6ef](https://github.com/nurtuo/nurtuo/commit/9a0a6ef4df55eeb0aa08de57bfaba7a21dd768f7)) |
| Issue | [#65](https://github.com/nurtuo/nurtuo/issues/65) |
| Code | [contracttest/largepayload.go](../../sdk/go/connector/contracttest/largepayload.go) |
| Parent spec | Gap 2 in [connector contract v1](implemented/2026-11-21-connector-contract-v1.md) |
| ADRs | None |

## Approved decisions

On 2026-11-25 at 19:09 Europe/Warsaw, Łukasz approved recording the decisions made via the
implementation merged in PRs #110, #111 and #113. They cover the opt-in API and in-process scope,
the configurable payload and heap ceiling, the sampled live-heap measurement, the requirement
for a single result for the streamed document, and the dedicated stream timeout.

## Options API

`contracttest.Options.LargePayload *LargePayloadCase` enables the
`contracttest.Run/ExecuteLargePayload` subtest with `Options.New`. The subtest skips when
`LargePayload` is nil or when the suite uses `Options.Dial`, since the measured heap belongs
to the test process.

| `LargePayloadCase` field | Behaviour |
|---|---|
| `Start *connectorv1.ExecuteStart` | Initial Execute message. The test clones a supplied value or starts with an empty message. It fills empty execution and step IDs with `contracttest-exec` and `contracttest-step` and uses `Options.Connection` if the message has no connection. |
| `Properties map[string]string` | Document properties sent on the first chunk. |
| `Size int` | Bytes in one document. Zero selects `DefaultLargePayloadSize` (64 MiB). The effective value must exceed 4 MiB. |
| `ChunkSize int` | Bytes per chunk. A nonpositive value selects `DefaultChunkSize` (64 KiB). |
| `MaxHeapGrowth uint64` | Allowed live heap growth in bytes. Zero selects `DefaultMaxHeapGrowth` (24 MiB). |
| `Want connectorv1.DocumentStatus` | Expected document status. `UNSPECIFIED` selects `SUCCESS`. |
| `Timeout time.Duration` | Deadline for the whole Execute stream. A nonpositive value selects `DefaultLargePayloadTimeout` (30 seconds). The stream uses this deadline independently of the general `Options.Timeout` default of 10 seconds, which a 64 MiB stream can exceed under `-race`. |

## Stream and heap measurement

1. Create the client and open the Execute stream. Before sending `ExecuteStart`, take the baseline
   by calling `runtime.GC()` and reading `HeapAlloc` with `runtime.ReadMemStats`.
2. Send one document with ID `contracttest-large` using one reusable chunk buffer. Mark the last
   chunk, send `ExecuteEnd`, and close the send side. While the stream runs, a goroutine samples
   every 5 ms using the same forced GC and `HeapAlloc` read.
3. Receive through EOF, requiring exactly one `DocumentResult` with the streamed document's ID
   and the configured status. Stop the sampler and take a final sample. Growth is the highest
   sampled live heap minus the baseline, with the peak initialised to the baseline. Fail when
   growth exceeds `MaxHeapGrowth`.

The subtest runs serially in the suite and measures the whole Go process, including the connector,
test client, transport and any in-process fixture. Sampling can miss allocations released between
samples.

## Scope and compatibility

The test SDK API is additive. SFTP opts in with a CREATE operation targeting `/in/large.bin`.
Its in-process fixture discards that file's data, counts the bytes written and verifies the
byte count after rename. HTTP buffers document bodies by design, NATS deals in size-limited
messages, and Postgres consumes JSON documents. Those connectors leave the case unset.
Out-of-process bridges are outside the measured heap. This is test coverage for gap 2 and adds
no runtime input limit or protocol change.

## Testing

| Test | Coverage | Command |
|---|---|---|
| `TestStreamingConnectorPassesLargePayload` | A streaming fake passes through `contracttest.Run`. | `go test -race ./sdk/go/connector/contracttest` |
| `TestLargePayloadMeasuresHeapGrowth` | Streaming stays within the default bound and retaining the payload exceeds it. | Same as above |
| `TestLargePayloadRejectsDuplicateResults` | Duplicate results for the streamed document are rejected. | Same as above |
| SFTP `TestContract` | The default-size upload stays within the heap bound and reaches the fixture in full. | `go test -race ./connectors/sftp -run '^TestContract$'` |

## Changelog

- 2026-11-24. Created as a draft for issue #65.
- 2026-11-25. Recorded Łukasz's approval of the decisions made via the merged implementation
  and aligned the spec with PRs #110, #111 and #113.
