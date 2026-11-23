# bridges/ (Apache-2.0)

Optional out-of-process connector hosts. Each bridge is intended to become a **separate signed
artifact** (OCI, connector registry; planned, see Packaging: today the runtime neither downloads nor
verifies bridges), started as a supervised child process by the Go launcher (`connectors/bridge`)
and spoken to over gRPC
(`proto/nurtuo/connector/v1`) on a Unix socket or a loopback TCP port.

| Dir | Stack | Build | Status |
|---|---|---|---|
| `jvm/` | Java 21, grpc-java; SAP JCo, JMS/IBM MQ, JDBC, Smooks EDI/AS2, Saxon HE, Camel | Gradle + `jlink` JRE (with and without bundled JRE) | SDK, server, plugin loader and tests written (see [jvm/README.md](jvm/README.md)); **not compiled or run** (no JDK was available) |
| `python/` | Python 3.12+, grpcio; ML, embeddings | uv + python-build-standalone | `nurtuo_bridge` package, examples and tests exist; verified (see [python/README.md](python/README.md)) |
| `dotnet/` | .NET 8+, Grpc.AspNetCore | `dotnet publish` self-contained | project, example and xunit tests written; **not built or run yet** (no .NET SDK was available) |

Bridges stay Apache-2.0 so customer-supplied proprietary libraries (SAP JCo, IBM MQ client,
Oracle/Db2/Teradata JDBC, Saxon PE/EE) can run inside them without touching AGPL code.
These libraries are **never bundled**; the bridge loads them from a customer-provided path or
downloads them only after the customer accepts their license.

The runtime side is the Go package `connectors/bridge` (launcher, supervisor, gRPC client). This
document is the normative description of the process protocol every bridge implements.

## Process protocol

A bridge is a normal executable. The runtime (launcher) starts it, waits for one handshake
line, then talks gRPC to it. The protocol is independent of the bridge's language.

### Environment (runtime to bridge)

The child does **not** inherit the runtime's environment (it may hold secrets). It receives a
small allowlist (`PATH`, `PATHEXT`, `SYSTEMROOT`, `SYSTEMDRIVE`, `WINDIR`, `COMSPEC`, `TEMP`,
`TMP`, `TMPDIR`, `HOME`, `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `LANG`, `LC_ALL`, `TZ`), the
`Env` entries of the bridge configuration, and:

| Variable | Meaning |
|---|---|
| `NURTUO_BRIDGE_LISTEN` | Where to listen: `unix:/abs/path/b.sock` or `tcp:127.0.0.1:0` (loopback only; port 0 = pick one). The launcher uses a fresh `0700` temp directory for unix sockets. |
| `NURTUO_BRIDGE_CONTRACT` | Comma separated list of acceptable contract packages in order of preference, e.g. `nurtuo.connector.v1`. |
| `NURTUO_BRIDGE_TOKEN` | Random per-process shared secret. If set, **every** RPC (including health) must carry it in the `x-nurtuo-bridge-token` metadata/header, otherwise the bridge answers `UNAUTHENTICATED`. Protects loopback TCP from other local processes. |
| `NURTUO_BRIDGE_WATCH_STDIN` | `1`: shut down gracefully when stdin reaches EOF (the runtime closed the pipe or died). |

### Handshake (bridge to runtime)

When the server listens, the bridge writes **exactly one line** to stdout and flushes:

```
NURTUO-BRIDGE|1|<network>|<address>|<contract>|<implementation>
```

| Field | Meaning |
|---|---|
| `NURTUO-BRIDGE` | Fixed prefix. |
| `1` | Handshake (process protocol) version. Independent of the connector contract version. |
| `network` | `unix` (address = socket path) or `tcp` (address = `host:port`, loopback only: the launcher rejects anything else). |
| `address` | Where the runtime dials. |
| `contract` | The contract package the bridge will speak. Must be one of those offered in `NURTUO_BRIDGE_CONTRACT`. |
| `implementation` | Optional free text, e.g. `python/0.1.0`. May contain `\|`. |

Example: `NURTUO-BRIDGE|1|tcp|127.0.0.1:53384|nurtuo.connector.v1|python/0.1.0`

Rules:

- stdout carries the handshake and nothing else a program depends on: anything after the first
  line is read and logged at debug level (and must be drained, which the launcher does). Bridges
  should redirect their own stdout to stderr after printing the handshake so that stray
  `print`/`Console.WriteLine` calls cannot interfere (the Python and .NET runners do).
- All logs go to stderr, which the launcher forwards to the runtime log. Never log secrets.
- The runtime waits `StartTimeout` (default 15 s) for the handshake. A process that exits first,
  prints a malformed line, a non-loopback address, an unsupported handshake version or a contract
  that was not offered makes the start fail.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Clean shutdown. |
| 2 | Cannot start (bad listen spec, cannot bind). |
| 3 | None of the contracts in `NURTUO_BRIDGE_CONTRACT` is supported (incompatible bridge). |
| other | Crash; the supervisor restarts it. |

### Transport

- Linux/macOS: Unix domain socket (default), fastest and not reachable from the network.
- Windows: loopback TCP with the token. grpcio has no Unix socket support on Windows and Windows
  named pipes are not available in all bridge stacks; named pipes are future work. The launcher's
  `auto` network picks `tcp` on Windows. Any platform can use `tcp` explicitly.
- No TLS: the transport is local to the machine; the token authenticates the runtime.

### Lifecycle

1. **Start**: launcher runs `command args...` with the variables above and waits for the handshake.
2. **Ready**: the handshake line is printed only after the server accepts connections. The bridge
   also serves the standard `grpc.health.v1.Health` service (`""` and
   `nurtuo.connector.v1.ConnectorService` are `SERVING`; `NOT_SERVING` during shutdown).
3. **Run**: the runtime dials lazily through a dialer that always resolves to the current child.
   One `*grpc.ClientConn` stays valid across restarts. RPCs issued while the child restarts wait
   for it (bounded by the caller's context).
4. **Crash and restart**: if the child exits unexpectedly it is restarted with exponential backoff
   (`RestartBackoff`, default 200 ms, doubled per restart, capped at 5 s), at most `MaxRestarts`
   (default 3) per `RestartWindow` (default 1 min). Beyond that the bridge is failed: `Done()`
   closes, `Err()` says why and RPCs fail fast. A batch that was in flight when the child died
   fails with a non-OK gRPC status, never with success; documents without a result are `FAILURE`.
5. **Stop**: the launcher closes the child's stdin (the portable "runtime is going away" signal;
   all runners treat EOF as shutdown), waits `StopTimeout` (default 5 s) for in-flight RPCs and
   exit, then kills the process. SIGTERM/SIGINT are honoured the same way when sent manually.
   The unix socket directory is removed.

Not covered yet: killing grandchildren of the bridge on Windows (job objects) and cgroup/memory
limits. Bridges must not spawn detached children.

### Execute stream semantics

Defined in the proto; summarized because bridge authors get it wrong most often:

1. First message is `ExecuteStart`; then `DocumentChunk`s (chunks of one document share
   `document_id`, `properties` only on the first chunk, last has `last=true`, documents are never
   interleaved); then `ExecuteEnd`.
2. For **every** input document the bridge sends exactly one `DocumentResult`
   (`SUCCESS | APPLICATION_ERROR | FAILURE`), plus any output `DocumentChunk`s (the output document
   ids are listed in `output_document_ids`). A per-document error is a result, not a gRPC error.
3. A non-OK gRPC status means the whole batch failed (`INVALID_ARGUMENT` for protocol violations,
   `INTERNAL` otherwise). The runner base classes do this for you.
4. Payloads are streamed: read input chunk by chunk and write output chunks of at most 64 KiB
   (configurable). The base classes keep one chunk of lookahead, so memory stays bounded.
   If an output stream fails halfway the document ends as `FAILURE` and its partial output chunks
   must be discarded by the receiver.

### Secrets

The runtime resolves secret references and injects the values **in memory** in
`ConnectionConfig.settings` on each call. Bridges must not persist or log them and must not echo
them: `Test` diagnostics, `DocumentResult.message`, gRPC status messages and log lines go through a
redactor that masks every secret value found in the settings (keys named like
`password|secret|token|api_key|credential|private_key|authorization`, or the connector's declared
secret keys; values shorter than 4 characters are not masked). Tracebacks are only logged when
no secrets are present. Customer-supplied proprietary libraries are loaded from a customer path and
never bundled.

## Contract version negotiation

There are three independent version numbers:

| What | Where | Meaning |
|---|---|---|
| Contract package | proto package `nurtuo.connector.v1`, handshake field `contract` | Wire compatibility. Only additive changes within `v1` (new fields, enum values, RPCs), enforced by `buf breaking`; anything else is a new package `nurtuo.connector.v2`. |
| Handshake version | handshake field 2 (`1`) | The process protocol in this document. |
| Connector version | `ConnectorDescriptor.version` (SemVer) | The connector's own version, reported by `Describe`. |

Negotiation happens once per process start: the runtime offers an ordered list in
`NURTUO_BRIDGE_CONTRACT`; the bridge picks the first entry it supports and prints it in the
handshake; if it supports none it exits with code 3 and the start fails with a clear error. A
runtime that later speaks `v2` offers `nurtuo.connector.v2,nurtuo.connector.v1` and gets whichever
the bridge supports; a bridge can support both by shipping both stubs and switching on the
negotiated value. The bridge package in the registry declares which contracts it supports, so the
runtime can refuse an incompatible download before starting it.

Consequences of additive-only evolution inside `v1` that bridge authors must respect:

- Ignore unknown fields and tolerate unknown enum values (protobuf keeps them as numbers).
- A runtime newer than the bridge may call an RPC the bridge does not know (for example `Listen`
  on a bridge without listeners): the generated base returns `UNIMPLEMENTED`, which callers treat as
  "capability not available". Declare what you support in `ConnectorDescriptor.capabilities` and
  `operations`.
- A bridge newer than the runtime may fill fields the runtime ignores; never make correct
  behaviour depend on the runtime reading them.

## Authoring a bridge connector

Pick the stack, implement the four operations, run the shared conformance suite.

### Python

```python
from nurtuo_bridge import (ApplicationError, Connector, ConnectionSettings, ExecuteContext, InputDocument,
                           Outcome, OutputDocument, TestResult, make_descriptor, operation, run)
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb

class Shout(Connector):
    secret_keys = frozenset({"api_key"})          # None: guess from key names

    def describe(self):
        return make_descriptor(id="shout", version="1.0.0", display_name="Shout",
                               operations=[operation(pb.OPERATION_TYPE_EXECUTE, custom_type="SHOUT")])

    def test(self, settings: ConnectionSettings) -> TestResult:
        return TestResult(True, "ok")             # never put secrets into the diagnostic

    def execute_document(self, ctx: ExecuteContext, doc: InputDocument) -> Outcome:
        text = doc.read(limit=1 << 20).decode()   # or doc.iter_chunks() / doc.open() to stream
        if not text:
            raise ApplicationError("empty document")          # -> APPLICATION_ERROR
        return Outcome(outputs=[OutputDocument(data=text.upper().encode())])   # other exceptions -> FAILURE
```

Run it with `python -m nurtuo_bridge mypkg.shout:Shout`. For different semantics (batching,
reordering) override `Connector.execute`. Details and the example connectors are in
[python/README.md](python/README.md).

### .NET

Derive from `ConnectorBase`, override `Describe`, `TestAsync` and `ExecuteDocumentAsync` (or
`ExecuteAsync`), and call `BridgeHost.RunAsync(new MyConnector())` from `Main`. See
`dotnet/Nurtuo.Bridge.Examples/EchoConnector.cs` and [dotnet/README.md](dotnet/README.md).

### Conformance tests

`sdk/go/connector/contracttest` runs against any bridge by dialing it (`Options.Dial`). From Go:

```go
b, _ := bridge.Start(ctx, bridge.Config{Command: "python", Args: []string{"-m", "nurtuo_bridge", "mypkg.shout:Shout"}})
defer b.Close()
contracttest.Run(t, contracttest.Options{
    Dial:    func(t testing.TB) *grpc.ClientConn { cc, _ := b.NewConn(); return cc },
    Execute: []contracttest.ExecuteCase{ /* documents and expected statuses */ },
})
```

`connectors/bridge/python_test.go` does exactly this for the Python echo example. It is skipped
when no Python with the bridge dependencies is found; set `NURTUO_TEST_PYTHON` to a virtualenv
interpreter (`pip install -e bridges/python[dev]`).

## Packaging

**Status: planned.** The intended distribution is one artifact per OS/arch, signed with cosign and
pushed as an OCI artifact, with a registry entry that declares the supported `connector/v*`
contracts. None of this exists yet: the runtime does not download bridges and does not verify
signatures; it starts the command named in the bridge configuration (see SECURITY.md). The recipes
below describe the intended packaging.

| Bridge | Build | Notes |
|---|---|---|
| python | `uv` + python-build-standalone: a relocatable interpreter plus a locked virtualenv (`uv sync --frozen`, `uv pip install .`), shipped as a directory; command is `<dir>/bin/python -m nurtuo_bridge <module:Class>`. Generated stubs are committed, no protoc is needed on the target. Connectors with native wheels (ML) are built per platform. | Bring your own connector module on `PYTHONPATH` / as an installed package. |
| dotnet | `dotnet publish -c Release -r <rid> --self-contained -p:PublishSingleFile=true`; command is the produced executable. Stubs are generated by `Grpc.Tools` at build time. | Customer assemblies load from a customer path. |
| jvm | Gradle + `jlink` JRE (with and without bundled JRE). | Planned. |

## Verification status of this directory

| Part | State |
|---|---|
| Python package, examples, tests | `pytest` (in-process gRPC server plus process-level tests) |
| Go launcher `connectors/bridge` | `go test -race` with a re-exec fake bridge and the real Python example through `contracttest.Run` |
| .NET project and tests | written, **never compiled or run**: no .NET SDK was available when this was written; expect small compile fixes (package versions, generated names such as `Descriptor_`) |
