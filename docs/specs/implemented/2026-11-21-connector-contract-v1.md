<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Connector contract v1 (`nurtuo.connector.v1`)

Status: implemented. Date: 2026-11-21 (written as built on 2026-11-22).

## TLDR

`nurtuo.connector.v1` is the one gRPC contract every connector speaks: native Go connectors, bridged
JVM/Python/.NET connectors and (later) WASM connectors. It has five RPCs (`Describe`, `Browse`,
`Execute`, `Test`, `Listen`); `Execute` is a chunked bidirectional stream with exactly one result per
input document. The Go SDK (`sdk/go/connector`) and the conformance suite
(`sdk/go/connector/contracttest`) implement four of the five: `Listen` is in the proto but not in
the SDK interface or the suite.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/20 (squash commit `e637dfd`); in review: https://github.com/nurtuo/nurtuo/pull/41 (renames message `Descriptor` to `ConnectorDescriptor`) |
| ADRs | none specific; the bridge transport is described in [bridges/README.md](../../../bridges/README.md) |
| Issues | https://github.com/nurtuo/nurtuo/issues/38 (C# cannot compile a message named `Descriptor`) |
| Code | `proto/nurtuo/connector/v1/connector.proto`, `sdk/go/gen/nurtuo/connector/v1/` (generated), `sdk/go/connector/connector.go`, `sdk/go/connector/contracttest/`, `examples/connector-minimal/` |
| User docs | [connector-authoring.md](../../connector-authoring.md), [proto/README.md](../../../proto/README.md) |

## Problem and goals

Integration platforms live or die by their connector catalogue. Connectors are written in several
languages (Go for the built-ins; Java for JMS/IBM MQ/SAP JCo; Python for data/ML; .NET for
Windows-only SDKs) and must look identical to the engine.

- Goals: one language-neutral contract; per-document outcomes (`SUCCESS`, `APPLICATION_ERROR`,
  `FAILURE`) so one bad record does not fail a batch; streamed payloads so large documents do not
  have to fit one gRPC message; secrets injected by the runtime, never stored in connector
  descriptors; a reusable conformance suite so every connector is checked against the wire
  contract, not only a Go interface.
- Non-goals (v1 as built): connector packaging and distribution, connector-side OAuth flows, a
  WASM host, push listeners through the SDK (see the draft spec `connector-listen-operation`).

## Design (as built)

**Proto.** `proto/nurtuo/connector/v1/connector.proto`, service `ConnectorService`:

| RPC | Shape | Purpose |
|---|---|---|
| `Describe(DescribeRequest) returns (DescribeResponse)` | unary | descriptor: id, SemVer version, `Host`, display name, connection JSON Schema (`google.protobuf.Struct`), operations, tracked properties, `Capabilities{streaming, max_batch_size, transactional}` |
| `Browse(BrowseRequest) returns (BrowseResponse)` | unary | list object types, or return request/response `Profile`s (`FORMAT_JSON_SCHEMA` / `FORMAT_XSD`) for one object type |
| `Execute(stream ExecuteRequest) returns (stream ExecuteResponse)` | bidi stream | run one operation over a batch of documents |
| `Test(TestRequest) returns (TestResponse)` | unary | "test connection"; `diagnostic` must be redacted |
| `Listen(stream ListenRequest) returns (stream ListenResponse)` | bidi stream | push listener with per-document `Ack`; optional, `UNIMPLEMENTED` allowed |

Enums: `Host` (`HOST_GO_NATIVE`, `HOST_BRIDGE_JVM`, `HOST_BRIDGE_PYTHON`, `HOST_BRIDGE_DOTNET`,
`HOST_WASM`), `OperationType` (`GET`, `QUERY`, `CREATE`, `UPDATE`, `UPSERT`, `DELETE`, `EXECUTE`,
`LISTEN`), `DocumentStatus` (`SUCCESS`, `APPLICATION_ERROR`, `FAILURE`). `custom_type` is only
valid with `OPERATION_TYPE_EXECUTE` (for example `SEND_IDOC`, NATS `JETSTREAM_PUBLISH`).

**Execute stream protocol** (normative comment in the proto):

1. The client sends exactly one `ExecuteStart` (execution id, step id, `ConnectionConfig`,
   operation type, custom type, object type, options).
2. The client sends zero or more documents as `DocumentChunk`s. Chunks of one document share a
   `document_id`, `properties` travel with the first chunk, the last chunk has `last=true`, and
   documents are not interleaved.
3. The client sends `ExecuteEnd` and half-closes.
4. The server sends exactly one `DocumentResult` per input document (a business error is a result,
   not a gRPC error) plus output `DocumentChunk`s, then closes with OK. A non-OK gRPC status fails
   the whole batch for a technical reason; documents without a result count as `FAILURE`.

**Go SDK.** `sdk/go/connector/connector.go` defines

```go
type Connector interface {
    Describe(ctx) (*connectorv1.Descriptor, error)
    Browse(ctx, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error)
    Execute(connectorv1.ConnectorService_ExecuteServer) error
    Test(ctx, *connectorv1.TestRequest) (*connectorv1.TestResponse, error)
}
```

`connector.Serve(l net.Listener, c Connector)` registers an adapter that embeds
`UnimplementedConnectorServiceServer`, so `Listen` always answers `UNIMPLEMENTED`.
`connector.ErrNotImplemented` is the sentinel for optional operations. Native connectors in
`connectors/{http,sftp,postgres,nats}` implement this interface and are compiled into the runtime
(`connhost.Builtins()`); the runtime calls them in process through the same Go types.

**Conformance suite.** `contracttest.Run(t, Options{New | Dial, Connection, Golden, Secrets,
Execute, Timeout})` serves the connector through the real `connector.Serve` over `bufconn` (or dials
a running bridge through `Options.Dial`) and checks:

- `Describe`: non-empty id, SemVer `version`, `host` set, at least one operation, no
  `UNSPECIFIED` operation type, `custom_type` only on `EXECUTE`, optional golden descriptor
  (`proto.Equal`).
- `Test`: none of `Options.Secrets` appears in `diagnostic`.
- `Execute`: for each `ExecuteCase`, documents chunked at `DefaultChunkSize` (64 KiB) or
  `ChunkSize`, one result per input document, required statuses from `Want`, no `UNSPECIFIED`.

**PR #41 (in review).** Renames the message `Descriptor` to `ConnectorDescriptor` because protoc's
C# generator gives every message class a static `Descriptor` property, which collides (CS0542) and
kept `bridges/dotnet` from compiling. Field numbers and the `DescribeResponse.descriptor = 1` field
are unchanged, so the binary wire format is identical; generated type names change in every
language. It is the first deliberate breaking change of a v1 contract and the worked example in
the draft spec `backward-compatibility-contract`.

## Contracts

| Contract | Stability | Notes |
|---|---|---|
| proto package `nurtuo.connector.v1` (messages, field numbers, RPCs) | stable, additive-only | the file header says "additive changes only within v1"; `buf breaking` (FILE rules, `buf.yaml`) runs on pull requests against `main` in the `proto` job of `.github/workflows/ci.yml` |
| Execute stream protocol (one Start, chunks, End, one result per document) | stable | defined in proto comments; partially asserted by `contracttest` |
| `Listen` semantics (ListenStart, then one `Ack` per document, `max_in_flight`) | experimental | no implementation, no test |
| Go SDK `connector.Connector`, `connector.Serve`, `connector.ErrNotImplemented` | experimental (Go module `sdk/go`, pre-1.0) | adding `Listen` to the interface would break every implementer; see the Listen draft |
| `contracttest.Options`, `ExecuteCase`, `Document`, `DefaultChunkSize` | experimental | used by every native connector's tests and by `connectors/bridge` |
| Generated Go/TS code in `sdk/go/gen`, `sdk/ts/proto/src/gen` | derived | regenerated by `make gen` (`buf.gen.yaml`); committed |
| Connection schema extension `x-nurtuo-secret: true` | stable | read by `connections.SecretPaths`; see the connections and secrets spec |

## Compatibility

PR #41 is a breaking source change for every language binding (Go `connectorv1.Descriptor` becomes
`connectorv1.ConnectorDescriptor`; Python `connector_pb2.Descriptor`; Java/.NET generated classes)
while the wire format stays the same. A bridge built against the old stubs keeps working against a
runtime built from the new ones and the reverse, because only the message name changed. Code that
uses the message's full name (`google.protobuf.Any`, gRPC reflection, JSON with `@type`) would see
a different name; nothing in the repository does that today. The PR carries the `buf skip breaking`
label. Migration for third-party Go connectors: replace the type name and regenerate stubs. v1 has
not been released, which is why the change is accepted inside `v1` instead of a `v2` package.

## Security

- Secrets: `ConnectionConfig.settings` carries resolved secret values in memory only; the
  connection JSON Schema in the descriptor carries references, never values. The runtime wraps
  every connector with `connhost.redacting` (scrubs `Test` diagnostics and result messages).
- Transport for out-of-process connectors: Unix socket in a fresh `0700` temp directory, or
  loopback TCP on Windows, plus a per-process token in `x-nurtuo-bridge-token` (see the bridges
  spec). `connector.Serve` itself adds no authentication: the caller owns the listener.
- Input limits: chunking avoids the gRPC default 4 MiB message limit per message, but nothing in
  the contract bounds a document's total size or the number of documents per batch except
  `Capabilities.max_batch_size`, which the engine does not read today.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestFakeConnectorConforms` in `sdk/go/connector/contracttest/contracttest_test.go` | the suite against an in-memory fake | `cd sdk/go && GOWORK=off go test ./connector/...` | |
| `contracttest.Run` subtests `Describe`, `Test`, `Execute/<case>` | descriptor sanity, redaction, per-document status | run from each connector's tests (`connectors/*/..._test.go`, `examples/connector-minimal/connector_test.go`) | `Execute` is skipped when no cases are given |
| `contracttest.Run` subtest `ExecuteLargePayload` | streaming larger than one message with bounded memory | `go test -race ./connectors/sftp -run '^TestContract$'` | opt-in for in-process connectors through `Options.LargePayload` |
| `contracttest.Run` subtest `CrashBehaviour` | connector dying mid-batch never reported as SUCCESS | — | `t.Skip`; covered for bridges only by `TestCrashMidBatchIsNeverSuccess` in `connectors/bridge/bridge_test.go` |
| `contracttest.Run` subtest `Listen` | ListenStart, push, Ack | — | `t.Skip` |
| `buf lint`, `buf breaking`, stale generated code | contract hygiene | `proto` job in `.github/workflows/ci.yml` | `breaking` only on `pull_request` events |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A flow needs an inbound JMS queue or a NATS subscription: the proto declares `Listen`, but `connector.Connector` has no `Listen`, `connector.Serve` answers `UNIMPLEMENTED`, `connectors/nats` documents it as not implemented and the JVM SDK (`ConnectorService.java`) does not implement it, so no connector can be a listener | high | SDK, connectors, bridges | draft spec `connector-listen-operation` |
| 2 | A connector buffering a 300 MB document can exhaust runtime memory | high | contracttest | [ExecuteLargePayload memory bound](2026-11-24-execute-large-payload-mem-bound.md) covers opt-in, in-process streaming connectors. SFTP opts in. Connectors without a case and out-of-process bridges are skipped. |
| 3 | A native Go connector panics or returns early mid-batch; for in-process connectors the suite's `CrashBehaviour` is skipped, so a connector that reports `SUCCESS` for documents it never processed passes conformance | medium | contracttest | implement `CrashBehaviour` for `Options.New` (inject a failing document reader) |
| 4 | A contributor renames a field inside `v1` and adds the `buf skip breaking` label as PR #41 did; nothing requires an ADR, an upgrade note or a spec reference, so the label becomes a habit | medium | process | deprecation protocol and label policy in the draft spec `backward-compatibility-contract` |
| 5 | `Capabilities.max_batch_size` is declared (postgres declares `MaxBatchSize: 1000` in `connectors/postgres/postgres.go`) but no Go code outside the connectors reads it, so a batch of 10 000 documents reaches the connector in one `Execute` | medium | engine/contract | enforce in the engine or mark the field advisory; add a contract case |
| 6 | A step has an invalid connection setting: the HTTP connector answers with a per-document `FAILURE` for every document, while SFTP, Postgres and NATS fail the whole `Execute` stream with a non-OK status (finding from the native connector specs, PR #45). The proto does not say which is right, so DLQ routing and retry behaviour differ by connector for the same mistake | medium | contract | state in the proto comment that configuration errors before the first document are a stream status (`INVALID_ARGUMENT`), and add a contract case |
| 7 | The descriptor's `host` and `version` are self-reported; a bridged connector can claim `HOST_GO_NATIVE` and any version, and nothing cross-checks the bridge registry entry because there is none | low | contract | check against the bridge package metadata once packaging exists |
| 8 | `connector-authoring.md` still says "The `nurtuo` binary does not yet register any connector", although `connhost.Builtins()` registers `http`, `sftp`, `postgres`, `nats` since PR #25 | low | docs | fix the sentence in the next docs pass |

## Improvement opportunities

- Golden descriptors for all four native connectors (`Options.Golden`) so an accidental schema
  change shows up as a test diff.
- A `Browse` subtest (object types list is non-empty when `supports_browse` is set).
- A Python/JVM/.NET port of the suite, or a single Go runner that dials every bridge, so the
  conformance bar is identical across languages (today the JVM and .NET SDKs ship their own
  harnesses: `ConnectorTestHarness.java`, `BridgeTests.cs`).

## References

- Open Mercato: `integration-extensions` (extension points for third-party integrations),
  `llm-provider-ports-and-adapters` (one port, many adapters, shared conformance).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`, including PR #41 (in review).
