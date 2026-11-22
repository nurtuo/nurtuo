<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: PostgreSQL connector

Status: implemented. Date: 2026-11-21.

## TLDR

`connectors/postgres` is the native Go connector on `pgx/v5`: every input document is
`{"sql": "...", "params": [...] | {...}}`, values are always bound as parameters, `QUERY` emits one
JSON document per row inside a database-enforced **READ ONLY** transaction, and writes report
`rows_affected`. The one thing to know: the SQL text itself comes from the document and runs with
the privileges of the configured database user. The read-only transaction is defence in depth, not a
privilege boundary, and the live test that proves it **has never run against a real server**.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/30 (squash commit `b3ccd55`; its last commit made `QUERY` read-only); https://github.com/nurtuo/nurtuo/pull/41 (open, renames the proto message `Descriptor` to `ConnectorDescriptor`, touches `Describe`) |
| ADRs | [ADR 0005](../../adr/0005-set-at-a-time-engine-with-per-document-status.md), [ADR 0010](../../adr/0010-connections-and-secrets.md) |
| Issues | none |
| Code | `connectors/postgres/` (`postgres.go`, `config.go`, `bind.go`, `connector.yaml`); registered in `core/runtime/internal/connhost/registry.go` (`Builtins`) |
| User docs | [docs/connectors/postgres.md](../../connectors/postgres.md) |

## Problem and goals

Flows read from and write to relational databases constantly (lookups, staging tables, sync
targets). They need parameterised statements that cannot be broken by data values, predictable
row-to-JSON mapping, optional all-or-nothing batches, and status codes that separate bad data from
an unavailable database.

- Goals: parameter binding only (positional `$1` or named `@name`), JSON output rows, optional
  per-batch transaction, read-only `QUERY`, bounded pool count, SQLSTATE-based status mapping,
  password never in messages.
- Non-goals (M1): SQL builders (insert/upsert from a document without SQL), streaming of large
  result sets, `COPY`, `LISTEN/NOTIFY`, logical replication / change data capture, `Browse` of
  tables and columns, statement allow-lists.

## Design (as built)

**Descriptor.** `(*Connector).Describe` in `connectors/postgres/postgres.go`: id `postgres`,
version `0.1.0`, host `HOST_GO_NATIVE`, display name `PostgreSQL`, operations `QUERY`, `CREATE`,
`UPDATE`, `DELETE` and `EXECUTE` with custom type `STATEMENT` (`CustomTypeStatement`), each with
the `optionsSchema` (`transaction`, `maxRows`, `timeoutSeconds`); tracked properties
`rows_affected`, `row_count`; `Capabilities{Streaming: false, MaxBatchSize: 1000, Transactional:
true}`. The connection schema is the inline JSON constant `connectionSchema`; `connector.yaml`
lists the fields for the catalogue (`schema: inline`). There is no `schemas/connection.json`.
`Browse` returns `connector.ErrNotImplemented`.

**Connection settings** (`ParseConfig` in `config.go`):

| Key | Default | Notes |
|---|---|---|
| `host`, `database`, `user` (required) | | |
| `port` | 5432 | 1..65535 |
| `password` | | `format: secret-ref`, resolved by the runtime |
| `sslMode` | `require` | `disable`, `require`, `verify-ca`, `verify-full` (`validSSLModes`) |
| `sslRootCert` | | **path** to a CA bundle on the runtime host |
| `poolMaxConns` | 4 | 1..100 |
| `connectTimeoutSeconds` | 10 | |

`Config.URL` builds an escaped `postgres://` URL (contains the password; only fed to
`pgxpool.ParseConfig`). **Pools** are cached per connection in `Connector.pools`, keyed by
`Config.key` (SHA-256 of the URL, so the plaintext password is not a map key), reference counted,
at most `maxPools` = 8 with least-recently-used eviction (`evictLocked`; an evicted pool closes
when its last user releases it). `Test` uses an uncached `ephemeralPool` and runs `select 1`.

**Input document** (`ParseStatement`, `BindParams` in `bind.go`): JSON `Statement{SQL, Params}`.
`params` as an array binds positionally; as an object it becomes `pgx.NamedArgs` (`@name`).
`convertParam` keeps integral numbers as `int64`, binds objects and arrays as JSON
(`json.RawMessage`), passes strings, booleans and null. Malformed JSON, empty `sql` or bad `params`
wrap `errInput`.

**Execute.** Unsupported operation types fail the stream with `codes.InvalidArgument` before any
document is read; then **all chunks of the batch are read into memory** (`docs`, `byID`), the
options are parsed (`transaction` false, `maxRows` 10000, `timeoutSeconds` 30 per statement), and a
pool is acquired (`Connector.pool`; invalid settings → `codes.InvalidArgument`).

- Without `transaction`: each document runs on its own with `context.WithTimeout`. `QUERY` uses
  `runReadOnly`: `BeginTx` with `readOnlyTx` (`pgx.TxOptions{AccessMode: pgx.ReadOnly}`), the
  statement, then always `Rollback`. Other operations call `runStatement` on the pool
  (autocommit).
- With `transaction: true`: one `BeginTx(txOptions(isQuery))` for the whole batch (read-only for
  `QUERY`); the first non-`SUCCESS` document stops the batch, the transaction is rolled back, that
  document keeps its own status and every other document becomes `FAILURE` ("transaction rolled
  back: document <id> failed"). A failed `BeginTx` or `Commit` makes every document `FAILURE`.
- `runStatement` uses `Query` for every operation. For `QUERY`, each row becomes a document via
  `RowToDocument` (`{column: value}`; `normalizeValue` renders UUIDs as strings, `numeric` as JSON
  numbers, `bytea` as base64, `interval`/`time` as text); more than `maxRows` rows is
  `APPLICATION_ERROR` and no rows are emitted. For the other operations rows (for example from
  `RETURNING`) are counted, not emitted. Tracked: `row_count`, `rows_affected` (command tag).
- Output rows are sent as documents `<id>/<n>`, each in **one** chunk with `Last: true`, followed
  by the `DocumentResult`.

The operation type (`CREATE`, `UPDATE`, `DELETE`, `STATEMENT`) is a label only: all four run the
document's SQL the same way. Only `QUERY` changes behaviour (read-only, rows emitted).

**Status mapping** (`MapError`):

| Outcome | Status |
|---|---|
| success | `SUCCESS` |
| SQLSTATE `25006` (write in a `QUERY`) | `APPLICATION_ERROR`, fixed message `write rejected: QUERY runs in a read-only transaction (SQLSTATE 25006)` with no server detail |
| SQLSTATE classes `21`, `22`, `23`, `28`, `2B`, `2D`, `3D`, `3F`, `42`, `44`, `P0` | `APPLICATION_ERROR` with `<message> (SQLSTATE <code>)` |
| `errInput` (malformed document), more than `maxRows` rows | `APPLICATION_ERROR` |
| `40001`, `40P01` (serialization, deadlock) and every other SQLSTATE; connection, timeout and context errors | `FAILURE` |
| unsupported operation, invalid settings | whole stream fails (`InvalidArgument`) |

## Contracts

| Contract | Stability |
|---|---|
| Connector id `postgres`, version `0.1.0`, the five operations above | experimental |
| Connection keys (inline `connectionSchema`) | experimental |
| Step options `transaction`, `maxRows`, `timeoutSeconds` | experimental |
| Input document `{"sql", "params"}` | experimental |
| Output rows `<id>/<n>` as JSON objects with the normalisation above | experimental |
| Tracked properties `rows_affected`, `row_count` | experimental |
| `QUERY` runs read-only (SQLSTATE 25006 → `APPLICATION_ERROR`) | experimental |
| Exported helpers `ParseConfig`, `ParseStatement`, `BindParams`, `RowToDocument`, `MapError` | internal |

## Compatibility

No contract change in this spec. Open https://github.com/nurtuo/nurtuo/pull/41 changes the return
type of `Describe` to `*connectorv1.ConnectorDescriptor`; wire format unchanged. Update this spec
when it merges.

## Security

- **SQL text is trusted input.** Values are bound, never concatenated, but the statement text comes
  from the document and is not restricted (package comment in `postgres.go`, user docs). A flow
  that maps untrusted input into `sql` is a SQL injection by construction.
- **Read-only `QUERY`.** Enforced by PostgreSQL through the transaction access mode; protects
  against accidental or injected writes in a `QUERY` step, not against a hostile flow author.
- **Least privilege.** Recommended: a read-only role for `QUERY` connections, a separate write role,
  no superuser, no `pg_execute_server_program`.
- **TLS.** Default `sslMode: require` encrypts but does **not** verify the server certificate;
  `verify-full` with `sslRootCert` is the recommended production setting. `disable` sends
  credentials and data in clear text.
- **Secrets.** `redact` removes the password, also in query- and path-escaped form, from every
  message; the pool key is a hash; the runtime `redacting` wrapper adds a second layer.
- **Limits.** `maxRows` per statement, `timeoutSeconds` per statement, `poolMaxConns` ≤ 100, at most
  8 pools.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestContract` in `connectors/postgres/postgres_test.go` | shared suite; without a database: Describe and Test (unreachable `127.0.0.1:1`, no secret leak), **Execute skipped**; with a database: QUERY with a bound param, bad SQL and bad input → `APPLICATION_ERROR` | `cd connectors && GOWORK=off go test -race ./postgres/...` | Execute cases only when `NURTUO_TEST_POSTGRES_URL` is set |
| `TestLiveRoundTrip` | transaction rollback (duplicate key → `APPLICATION_ERROR` + `FAILURE`), insert `rows_affected`, query output, **INSERT in QUERY rejected with 25006 with and without `transaction`**, UPDATE still works | `NURTUO_TEST_POSTGRES_URL=postgres://user:pw@localhost:5432/db?sslmode=disable go test -race ./postgres/...` | skipped without the env var (`liveSettings`); never run so far |
| `TestBindPositional`, `TestBindNamedAndJSON`, `TestBindErrors` | parameter binding | default | |
| `TestRowToDocument`, `TestMapError`, `TestConfig`, `TestTestRedactsPassword` | row mapping, status mapping, config validation, redaction | default | |
| `TestTxOptions`, `TestQueryRunsInReadOnlyTxAndRollsBack`, `TestMapErrorReadOnly` in `readonly_test.go` | read-only options, rollback, fixed 25006 message (with `fakeTx`/`fakeBeginner`) | default | fakes, no server |
| `TestPoolKeyHasNoPlaintextPassword`, `TestPoolsAreBoundedAndEvicted`, `TestTestLeavesNoPool`, `TestInvalidConfigIsInvalidArgument`, `TestInvalidOperationTypeRejected` in `findings_test.go` | pool cache, error codes | default | |

`.github/workflows/ci.yml` provides no PostgreSQL service and does not set
`NURTUO_TEST_POSTGRES_URL`, so in CI the connector's Execute path is covered only by fakes.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A flow maps an HTTP listener body field into `sql` (or builds it with a JSONata string concatenation). A caller sends `"sql": "drop table invoices"` to a `DELETE`/`EXECUTE` step and the connector runs it with the connection's privileges. | high | security | Named statements: SQL text in the step configuration (validated at deploy), only `params` from the document; optional allow-list of statement kinds per operation; flow validation warning when `sql` is mapped from a listener. |
| 2 | The read-only guarantee and the 25006 mapping were only tested with fakes; `TestLiveRoundTrip` never ran, and CI has no database. A pgx or server behaviour difference would ship unnoticed. | high | testing | Add a PostgreSQL service to CI and set `NURTUO_TEST_POSTGRES_URL`; run the live tests on every connectors change. |
| 3 | A `QUERY` step with `transaction: true` receives two documents: `{"sql":"set transaction read write"}` (first statement of the transaction) then an `INSERT`. PostgreSQL may accept the mode change before the first snapshot, and the insert is committed. Not covered by a test. | medium | security | Live test for this sequence; reject `SET TRANSACTION`/`set_config('transaction_read_only', ...)` in `QUERY`, or run the batch as a read-only role (`SET ROLE`) instead of relying on the access mode. |
| 4 | Default `sslMode: require` on a cloud network: an attacker on the path presents any certificate, the connection proceeds, and query results and the session are exposed. `sslRootCert` is a file path, so a CA bundle cannot come from the secret store; containers need a mounted file. | medium | security | Default to `verify-full` for new connections (keep `require` for existing ones with a warning in `Test`); accept CA PEM content (`sslRootCertPem`) as a setting. |
| 5 | A `QUERY` returns 10000 rows with a 1 MB `jsonb` column each: `runStatement` keeps every row in `out.rows` before sending, and the whole input batch is buffered beforehand, so ~10 GB is held in memory and the runtime is OOM-killed. Each row is one chunk with no size cap. | medium | reliability | Stream rows to the output as they are read (chunked), add a byte cap per document and per batch (as the NATS connector does with 64 MiB). |
| 6 | The database password is rotated before the connection is updated: every document fails with SQLSTATE `28P01`, which `MapError` maps to `APPLICATION_ERROR` (class `28`), so documents go to the error path instead of being retried as a technical failure. | medium | status mapping | Map class `28` to `FAILURE`, like class `08` (connection exceptions) already is. |
| 7 | A table with 2 million rows cannot be extracted: more than `maxRows` rows is an `APPLICATION_ERROR` with zero rows, and there is no cursor or pagination. | medium | usability | Server-side cursors (`DECLARE ... FETCH`) with one output batch per page, or keyset pagination options. |
| 8 | A 30 s `timeoutSeconds` cancels a heavy report query; the context error is `FAILURE`, the runtime retries, and the same heavy query hits the database again on every attempt. | low | reliability | Map `57014` (query canceled by timeout) to a distinct message; let flows mark timeouts as non-retryable. |
| 9 | `connector.yaml` describes `CREATE` as "Insert a row from the current document", but `CREATE` needs `{"sql", "params"}` like every other operation; a `DELETE` step can run `DROP TABLE`. The advertised `MaxBatchSize: 1000` is not enforced in `Execute`. | low | docs, contract | Fix the descriptor text; enforce or drop `MaxBatchSize`; decide whether operation types should constrain the statement kind. |

## Improvement opportunities

- **Bulk insert / upsert**: `CREATE`/`UPSERT` from plain JSON documents with a `table` and
  `conflictColumns` option, executed as one multi-row `INSERT ... ON CONFLICT` or `COPY FROM` per
  batch.
- Streaming `QUERY` with server-side cursors and pagination tokens; incremental sync by a
  watermark column (`updated_at > $last`) stored per flow.
- `Browse` of schemas, tables and columns to generate JSON Schema profiles for the canvas.
- `LISTEN/NOTIFY` or logical replication as a `Listen` source (CDC).
- Statement-level metrics as tracked properties (`duration_ms`).
- `pgx.QueryExecModeExec` option for poolers (PgBouncer transaction mode) that break prepared
  statements.

## References

- Open Mercato `data-sync-hub`: incremental data sync, cursors and retry/resume; ideas for
  watermark-based extraction and bulk upsert.

## Changelog

- 2026-11-22: created (as built, from `main` at `3d3d506`).
