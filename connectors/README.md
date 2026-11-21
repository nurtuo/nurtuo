# Nurtuo native connectors (Apache-2.0)

Native Go connectors compiled into the runtime. Each directory holds a `connector.yaml`
descriptor, JSON Schemas under `schemas/`, the Go implementation and contract tests.
M1 set: `http` (REST + OpenAPI import), `sftp`, `postgres`, `nats`.

Connectors depend only on `sdk/go`, never on `core/`.
