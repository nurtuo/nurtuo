<!-- SPDX-License-Identifier: Apache-2.0 -->
# Writing a connector (Go)

A connector adapts one external system to the `nurtuo.connector.v1` gRPC contract
(`proto/nurtuo/connector/v1/connector.proto`). This page covers the Go SDK
(`github.com/nurtuo/nurtuo/sdk/go`, Apache-2.0) and the reusable conformance suite
`sdk/go/connector/contracttest`. A complete connector that compiles and passes the suite lives in
[`examples/connector-minimal`](../examples/connector-minimal/connector.go).

Verified while writing this page: `go test github.com/nurtuo/nurtuo/examples/connector-minimal`
passes (both inside the Go workspace and standalone with `GOWORK=off`).

## What the runtime offers today

- The Go SDK and the contract test suite work as described below.
- Native connectors are compiled into the runtime. The `nurtuo` binary does not yet register any
  connector with the engine, so a connector you write cannot yet be selected by a deployed flow.
- Out-of-process connectors (and bridges for JVM, Python, .NET) are specified (gRPC over a Unix
  socket or Windows named pipe, supervised by the runtime) but the runtime's bridge host
  (`internal/bridgehost`) is a placeholder. `connector.Serve` runs your connector as a gRPC server
  on a listener you give it.
- The SDK interface has no `Listen` method yet; connectors cannot be inbound listeners through the
  Go SDK.

## The interface

```go
type Connector interface {
	Describe(ctx context.Context) (*connectorv1.Descriptor, error)
	Browse(ctx context.Context, req *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error)
	Execute(stream connectorv1.ConnectorService_ExecuteServer) error
	Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error)
}
```

`connector.ErrNotImplemented` is the conventional error for optional operations (`Browse`).
`connector.Serve(listener, c)` registers the connector with a gRPC server and blocks until the
listener closes.

### Describe

Return a `Descriptor`: `Id`, `Version` (SemVer), `Host` (`HOST_GO_NATIVE` for Go connectors),
`DisplayName`, an optional `ConnectionSchema` (JSON Schema as a protobuf `Struct`; secrets are
references, never values), the `Operations` (`Type`, plus `CustomType` only for `EXECUTE`),
`TrackedProperties` and `Capabilities` (`Streaming`, `MaxBatchSize`, `Transactional`). Keep it
equal to your `connector.yaml`. The suite checks that the id is set, the version is SemVer, the
host is specified, at least one operation is declared and `custom_type` is used only with `EXECUTE`.

### Execute: the stream protocol

1. First message: exactly one `ExecuteStart` (execution id, step id, connection, operation type,
   custom type, object type id, options).
2. Zero or more documents as `DocumentChunk`s. The chunks of one document share a `document_id`;
   properties arrive with the first chunk; the last chunk has `last = true`; documents are not
   interleaved.
3. `ExecuteEnd`, then the client half-closes the stream (your `Recv` returns `io.EOF`).
4. For every input document send exactly one `DocumentResult`, plus any output documents as
   `ExecuteResponse_Output` chunks, then return `nil`.

The status per document:

| Status | Use for |
|---|---|
| `DOCUMENT_STATUS_SUCCESS` | done; list `OutputDocumentIds` if you emitted output documents |
| `DOCUMENT_STATUS_APPLICATION_ERROR` | the target system rejected the document (business error) |
| `DOCUMENT_STATUS_FAILURE` | technical failure (connection, timeout, crash) |

A per-document problem is a result, not a gRPC error. Returning a non-OK error from `Execute`
means the whole batch failed technically: the runtime treats documents without a result as
`FAILURE`. Report values for Process Reporting in `DocumentResult.Tracked`; the engine exposes them
as document properties `tracked.<name>`. When a result lists output document ids, the engine
replaces the input document by those outputs (input properties are inherited, output properties
win); with `SUCCESS` and no outputs the input passes through unchanged.

### Test

Return `Ok` and a short `Diagnostic`. The diagnostic must never contain secrets; the suite checks
this for the values you list in `Options.Secrets`. Redact secrets from every error message you
return, as the native connectors do.

## A complete minimal connector

`examples/connector-minimal/connector.go` upper-cases payloads. The core:

```go
func (*Connector) Describe(context.Context) (*connectorv1.Descriptor, error) {
	return &connectorv1.Descriptor{
		Id: "uppercase", Version: Version, Host: connectorv1.Host_HOST_GO_NATIVE,
		DisplayName: "Uppercase (example)",
		Operations: []*connectorv1.OperationDescriptor{
			{Type: connectorv1.OperationType_OPERATION_TYPE_CREATE},
		},
		TrackedProperties: []string{"bytes"},
	}, nil
}
```

`Execute` reads the `ExecuteStart`, collects chunks per document id until `ExecuteEnd`, then for
each document: empty payload becomes `APPLICATION_ERROR`, invalid UTF-8 becomes `FAILURE`, anything
else sends an output chunk `<id>.out` with the upper-cased bytes and a `SUCCESS` result with the
tracked property `bytes`. Read the file; it is short.

The module `github.com/nurtuo/nurtuo/examples/connector-minimal` depends only on
`github.com/nurtuo/nurtuo/sdk/go` (never on `core/`) and has a `replace` to the sibling path so it
builds in this repository.

## Testing with contracttest

The suite serves your connector through the real gRPC server (`connector.Serve`) over an in-memory
listener and drives it through the generated client, so it checks the wire contract, not only the
Go interface.

```go
func TestContract(t *testing.T) {
	contracttest.Run(t, contracttest.Options{
		New: func(testing.TB) connector.Connector { return minimal.New() },
		Execute: []contracttest.ExecuteCase{{
			Name:  "per-document status",
			Start: &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE},
			Documents: []contracttest.Document{
				{ID: "ok", Data: []byte("hello")},
				{ID: "empty"},
				{ID: "large", Data: make([]byte, 200_000)}, // several chunks
			},
			Want: map[string]connectorv1.DocumentStatus{
				"ok":    connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
				"empty": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
				"large": connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
			},
		}},
	})
}
```

`Options` fields:

| Field | Meaning |
|---|---|
| `New` | returns a fresh connector per subtest (use this or `Dial`, exactly one) |
| `Dial` | returns a `*grpc.ClientConn` to an already running connector, for bridges |
| `Connection` | `ConnectionConfig` passed to `Test`, `Browse` and `Execute` |
| `Golden` | if set, `Describe` must return exactly this descriptor |
| `Secrets` | values that must never appear in the `Test` diagnostic |
| `Execute` | cases: `Name`, `Start`, `Documents` (`ID`, `Properties`, `Data`), `Want` (document id to status), `ChunkSize` |
| `Timeout` | per RPC, default 10 s |

What the suite checks today: descriptor sanity (and the golden descriptor), `Test` secret
redaction, and for each Execute case that every input document gets exactly one result with a
specified status, results only for known documents, the expected status where `Want` lists one,
and that the stream ends with an OK status. Documents are streamed in chunks of `ChunkSize`
(default 64 KiB), so a payload above that exercises chunk handling.

Skipped placeholders (reported as `SKIP` in the output): large payload streaming with bounded
memory, crash behaviour (a connector dying mid-batch must surface as non-OK, never `SUCCESS`),
and `Listen` acknowledgements. They are not checked yet.

Run it:

```sh
go test -v github.com/nurtuo/nurtuo/examples/connector-minimal
```

## Running a connector out of process

```go
l, err := net.Listen("unix", "/tmp/uppercase.sock") // or a named pipe listener on Windows
if err != nil { log.Fatal(err) }
log.Fatal(connector.Serve(l, minimal.New()))
```

This snippet was not run; `connector.Serve` itself is exercised by contracttest over an in-memory
listener. The runtime-side handshake that hands a listener to a connector process is not
implemented yet.

## Checklist for a new connector

- Descriptor id, SemVer version, host and operations; a `connector.yaml` and JSON Schema for the
  connection (`schemas/connection.json`) next to the code, as the native connectors have.
- One result per document; per-document errors as results; correct `APPLICATION_ERROR` versus
  `FAILURE` split.
- Stream large payloads in chunks; do not hold whole documents longer than needed.
- No secrets in messages, diagnostics, tracked properties or logs. Test with `Options.Secrets`.
- Document the security properties (what a flow author can make the connector do) next to the
  operations, as in [connectors/postgres.md](connectors/postgres.md) and
  [connectors/http.md](connectors/http.md).
- Licensing: connectors on the Apache-2.0 SDK may use any license. Run closed-source connectors out
  of process rather than compiling them into the AGPL runtime binary (see
  [LICENSING.md](../LICENSING.md)). Files in this repository need an SPDX header matching the
  directory.
