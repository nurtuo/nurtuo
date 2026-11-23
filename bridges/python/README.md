# bridge-python (Apache-2.0)

Python connector host for ML/embeddings and any connector better written in Python. The
`nurtuo_bridge` package implements the bridge process protocol described in
[../README.md](../README.md) over gRPC (`grpcio`) and gives connector authors a small base class.

## Layout

```
src/nurtuo_bridge/
  connector.py      Connector base class (describe / test / browse / execute / execute_document), Outcome, Result
  documents.py      lazy InputDocument (iter_chunks / open / read), OutputDocument, chunk_output
  secrets.py        Secret, ConnectionSettings, Redactor
  server.py         ConnectorServicer, serve(), run() (handshake, signals, stdin watch), CLI loader
  examples/         echo.py (ECHO, UPPERCASE) and csv_reader.py (QUERY, browse, CSV_TO_JSONL)
  _gen/             generated protobuf/gRPC stubs (committed)
scripts/gen_proto.py  regenerates _gen from ../../proto
tests/              pytest: in-process server, process-level handshake/shutdown tests
```

## Develop

```
python -m venv .venv && . .venv/bin/activate        # Windows: .venv\Scripts\activate
pip install -e .[dev]
pytest
ruff check . && mypy src
python scripts/gen_proto.py            # regenerate stubs after changing proto/ (needs grpcio-tools)
python scripts/gen_proto.py --check    # CI: fail if committed stubs are stale
```

The committed stubs were generated with grpcio-tools 1.85 / protobuf 7.35 and need
`grpcio>=1.85.3` and `protobuf>=7.35.1` at runtime (set in `pyproject.toml`); older runtimes fail
at import with a clear version error.

## Run an example as a bridge

```
NURTUO_BRIDGE_LISTEN=tcp:127.0.0.1:0 python -m nurtuo_bridge nurtuo_bridge.examples.echo:EchoConnector
NURTUO-BRIDGE|1|tcp|127.0.0.1:53384|nurtuo.connector.v1|python/0.1.0      <- stdout (handshake)
```

Normally the runtime starts it (`connectors/bridge`). Ctrl-C, SIGTERM, or closing stdin (with
`NURTUO_BRIDGE_WATCH_STDIN=1`) shut it down gracefully (`--grace` seconds for in-flight RPCs).

## Writing a connector

Subclass `Connector`:

- `describe()` returns a `ConnectorDescriptor` (`make_descriptor`, `operation` helpers; host is set to
  `HOST_BRIDGE_PYTHON`).
- `test(settings)` returns `TestResult(ok, diagnostic)`; exceptions become `ok=False` with a
  redacted message.
- `browse(ctx)` (optional) returns `BrowseResult(object_types=[(id, label)], request_profile=,
  response_profile=)`; the default maps to gRPC `UNIMPLEMENTED`.
- `execute_document(ctx, doc)` returns an `Outcome(status, message, tracked, outputs)`. Raise
  `ApplicationError` for business rejections (`APPLICATION_ERROR`); any other exception fails only
  that document (`FAILURE`). `doc` is a stream: `doc.iter_chunks()`, `doc.open()` (file object) or
  `doc.read(limit=...)`. Outputs are `OutputDocument(data=bytes | iterable[bytes], properties=...)`;
  a generator keeps memory bounded. `Outcome.tracked` may be a callable evaluated after streaming.
- Override `execute(ctx, documents)` instead for batch-level behaviour: yield exactly one
  `Result` per input document plus output chunks (`documents.chunk_output`).

Secrets: `ctx.settings.secret("api_key").reveal()` at the point of use. Everything returned to the
runtime (results, diagnostics, status messages, logs) is redacted against the secret values in the
settings. Numbers in `settings`/`options` are JSON doubles (protobuf `Struct`).

Environment variables, handshake, exit codes and the Execute stream rules are in
[../README.md](../README.md).
