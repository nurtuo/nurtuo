<!-- SPDX-License-Identifier: Apache-2.0 -->
# PostgreSQL connector (`postgres`)

Native Go connector on `pgx/v5`. Source: `connectors/postgres`, descriptor
`connectors/postgres/connector.yaml` (the connection schema is inline, in `postgres.go`).
Version `0.1.0`, host `go-native`, `transactional: true`, `maxBatchSize: 1000`, not streaming.

Status: tested against a PostgreSQL server in the connector tests and registered in the `nurtuo` binary, so a deployed flow can use it through a bundle connection (secrets resolved from the runtime secret store, see [operations.md](../operations.md)). Not run against a production deployment.

## Security: arbitrary SQL, read-only QUERY

**The SQL text of every statement comes from the input document and is executed as is with the
privileges of the configured database user.** There is no allow-list. Values are bound as
parameters, but the statement text is not restricted. The operation type (`QUERY`, `CREATE`, ...)
is not checked against the SQL.

**`QUERY` is read-only, enforced by the database.** Every `QUERY` document runs inside a
`READ ONLY` transaction (pgx `TxOptions{AccessMode: ReadOnly}`, rolled back at the end; with the
`transaction` option all documents share one read-only transaction). A write attempted in a
`QUERY` step (`INSERT`, `UPDATE`, `DELETE`, DDL, ...) is rejected by PostgreSQL with SQLSTATE
`25006` (`read_only_sql_transaction`) and that document is reported as `APPLICATION_ERROR` with
the message `write rejected: QUERY runs in a read-only transaction (SQLSTATE 25006)`, without
connection details. `CREATE`, `UPDATE`, `DELETE`, `EXECUTE` and the `transaction` option behave as
before.

**What this does not protect against:**

- Reads. A `QUERY` can read everything the database user may read.
- `CREATE`, `UPDATE`, `DELETE` and `EXECUTE` steps still run any statement the user may run,
  including `DELETE`, `DROP`, DDL and, if the role is allowed, server-side features such as
  `COPY ... PROGRAM`.
- A hostile flow author. The read-only mode is a transaction setting of the session, not a
  privilege; a statement that switches the transaction back to read-write may defeat it. It
  protects against accidental or injected writes in a `QUERY` step, not against someone who may
  author arbitrary SQL. The live-database test that asserts an `INSERT` in a `QUERY` is rejected
  only runs when `NURTUO_TEST_POSTGRES_URL` is set; it has not been run against a real server yet.

Recommended: use a **dedicated least-privilege database account per purpose**. Give `QUERY`-only
flows a role that has only `SELECT` on the tables it needs (a real read-only role is the actual
boundary; the read-only transaction is defence in depth), no superuser, no
`pg_execute_server_program`; use a separate account with write grants for steps that write; use a
dedicated database or schema; control who may build flows and who may call HTTP listeners that feed
this step. Never build the SQL text from untrusted input; put untrusted values in `params`.

## Operations

| Operation | Behaviour |
|---|---|
| `QUERY` | runs the statement in a read-only transaction (writes are rejected, `APPLICATION_ERROR`); one output document `<id>/<n>` per row, a JSON object `{column: value}` |
| `CREATE`, `UPDATE`, `DELETE` | runs the statement; rows are counted, not emitted (including `RETURNING` rows) |
| `EXECUTE` with custom type `STATEMENT` | same as above for arbitrary statements |

Any other operation (for example `GET`) fails the whole stream with `InvalidArgument`. `Browse` is
not supported.

Input document (JSON payload):

```json
{ "sql": "select id, name from employee where id = $1", "params": [42] }
{ "sql": "update employee set name = @name where id = @id", "params": { "name": "Ada", "id": 42 } }
```

`params` is an array (positional `$1`...) or an object (named `@name`). Integral numbers keep
integer precision; objects and arrays are bound as JSON. Types are normalised in output rows
(UUIDs as strings, `numeric` as JSON numbers, `bytea` as base64).

Step options: `transaction` (boolean, default false), `maxRows` (default 10000, QUERY only),
`timeoutSeconds` (per statement, default 30). With `transaction: true` all documents of the batch
run in one transaction: if one fails the transaction is rolled back and every document is reported
as `FAILURE` (the failing one with its own status and message).

## Connection settings

| Setting | Type | Default |
|---|---|---|
| `host`, `database`, `user` | string (required) | |
| `port` | integer 1..65535 | 5432 |
| `password` | string, secret reference resolved by the runtime | |
| `sslMode` | `disable`, `require`, `verify-ca`, `verify-full` | `require` |
| `sslRootCert` | path to a CA bundle | |
| `poolMaxConns` | integer 1..100 | 4 |
| `connectTimeoutSeconds` | integer | 10 |

Pools are cached per distinct connection (keyed by a hash of the URL, so the password is not kept
as a key), at most 8 at a time; the least recently used idle pool is closed beyond that.
`sslMode: disable` sends credentials and data in clear text; use `verify-full` with `sslRootCert`
in production (`require` encrypts but does not verify the server certificate).

## Tracked properties

`rows_affected` (command tag rows affected) and `row_count` (rows returned by the statement),
exposed as `tracked.rows_affected`, `tracked.row_count`.

## Status mapping

| Outcome | Status |
|---|---|
| success | `SUCCESS` |
| SQLSTATE classes 21, 22, 23, 28, 2B, 2D, 3D, 3F, 42, 44, P0; malformed input document; result over `maxRows` | `APPLICATION_ERROR` |
| other SQLSTATEs, including 40001/40P01 (serialization, deadlock: retryable), connection and timeout errors | `FAILURE` |
| invalid connection settings | the whole stream fails (`InvalidArgument`) |

Messages and diagnostics never contain the password. Test connection runs `select 1` on an
uncached pool and reports host, port, database and `sslmode` on success.

## Limits

`maxRows` 10000 per QUERY statement by default; the batch is read fully into memory before it runs
(not streaming); `maxBatchSize` 1000 documents is advertised in the descriptor.
