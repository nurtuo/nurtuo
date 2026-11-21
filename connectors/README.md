# Nurtuo native connectors (Apache-2.0)

Native Go connectors compiled into the runtime. Each directory holds a `connector.yaml`
descriptor, JSON Schemas under `schemas/`, the Go implementation and contract tests.
M1 set: `http` (REST + OpenAPI import), `sftp`, `postgres`, `nats`.

Connectors depend only on `sdk/go`, never on `core/`.

## Security notes

- `postgres` executes arbitrary SQL supplied in input documents with the privileges of the configured database user. `QUERY` runs in a READ ONLY transaction enforced by the database (a write is rejected with SQLSTATE 25006, `APPLICATION_ERROR`); this does not limit reads and does not apply to `CREATE`/`UPDATE`/`DELETE`/`EXECUTE`. Use a dedicated least-privilege account (read-only for `QUERY` steps) and restrict who can author flows using the connection.

`bridge/` is not a connector but the launcher that runs out-of-process bridges (JVM, Python, .NET; see ../bridges/README.md) and exposes them as a `connector.Connector`.
