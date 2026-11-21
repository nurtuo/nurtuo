# ADR 0010: Connections, secrets and the connector registry in the runtime

Date: 2026-11-21. Status: accepted.

**Context.** A flow's `connector` step names a connector (`http`) and optionally a `connection`. The
production runtime had no connector registered, no connection configuration and no way to obtain
secrets, so connector steps could not run outside tests. The runtime must stay a single static
binary that works standalone and distributed, and secrets must never travel in bundles, flows,
logs, status messages or `Describe` output.

## Decision

### (a) Connections are bundle components; secrets are references

* A **connection** is `connector id + non-secret settings + secret references`. It ships in the
  bundle as component kind `connection` (signed like flows):
  `{"schema":"nurtuo.connection/v1","id":"erp-api","connector":"http","settings":{"baseUrl":"https://erp.example","auth":{"type":"bearer","token":"secret://erp-token"}}}`.
* A secret reference is a settings **string value** of the form `secret://<name>` anywhere in the
  settings tree. The resolver replaces it in memory just before the connector is created. A
  connection never contains a secret value. Fields the connector's connection schema marks
  `x-nurtuo-secret: true` must be a `secret://` reference; a literal there fails validation
  (so a password can't be committed into a bundle by accident).
* A flow step references a connection by id: `{"connector":"http","connection":"erp-api",...}`.
  The id is resolved within the deploying bundle. The connection's `connector` must equal the step's
  `connector`. If `connection` is omitted the bundle must hold exactly one connection for that
  connector (the default); zero or several is a deploy error. Connection ids are scoped to the
  deployment: two deployments may use the same id with different settings.
* Secret names match `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}` (no path separators, so a name can never
  address a file outside a provider's directory).

### (b) `SecretProvider`

`internal/secrets` defines `Provider{ Get(ctx, name) (Secret, error) }` and `Store` (a Provider that
also does `Set`, `Delete`, `List`). `Secret` is an opaque type: `String()`, `GoString()`, `%v`,
JSON and slog all render `***`; only `Reveal()` yields the value. `ErrNotFound` is the sentinel for
a missing secret; errors carry the name, never a value. Providers:

| provider | config | store | notes |
|---|---|---|---|
| `env` (default) | none | read-only | `secret://erp-token` reads `NURTUO_SECRET_ERP_TOKEN` (upper-case, non `[A-Z0-9]` to `_`). |
| `dir` | `path` | read/write | one file per secret. On POSIX the directory and files must not be accessible by group/other (0700/0600), symlinks and non-regular files are refused. On Windows file ACLs are not inspected (documented gap; the data dir inherits the user profile ACL). One trailing newline is trimmed (Kubernetes/Docker secret mounts). |
| `encrypted-file` | `path` (dir), optional `key_file` | read/write | single `secrets.enc`, AES-256-GCM, random nonce per write, header as AAD, atomic replace. Used by standalone mode. |

The master key of `encrypted-file` comes from a `KeySource`: first the OS keychain (a stub that
reports "unavailable" today, so behaviour is deterministic), then a 32-byte key file created with
0600 beside the store in the data dir (or `key_file`). This protects against a leaked/backed-up
`secrets.enc`, not against an attacker with full access to the data dir; the OS keychain closes that
gap once implemented.

A `Redactor` collects every resolved secret value; it scrubs error strings, status messages,
connector `Test` diagnostics and (through a `slog.Handler` wrapper) log records. It is defence in
depth on top of connectors already not echoing secrets.

Runtime config: `secrets: {provider: env|dir|encrypted-file, path: ..., key_file: ...}`; omitted
means `env`.

### (c) Connector registry and instances

* `connhost.Registry` maps connector id to a **factory**. `connhost.Builtins()` registers the native
  connectors `http`, `sftp`, `postgres`, `nats` at startup (`cmd/nurtuo` builds it; there is no
  placeholder any more). Bridge connectors will register the same way from config later (a factory
  returning a gRPC-backed `connector.Connector`), so the engine never distinguishes them.
* `connhost.Manager` creates **one instance per (deployment, connection)**: a fresh connector from
  the factory with the resolved `ConnectionConfig`. Pools live inside the instance (postgres keeps its
  pool per connector), so connections do not share state across deployments.
* Lifecycle on deploy: validate references, resolve secrets, create instances, `Test` every
  connection that a connector step references (default `strict`: a failing test fails the
  deployment, `warn`: reported but deployed, `off`). Only if all pass are the instances swapped in
  for that deployment, and the previous instances closed. On undeploy (or failed redeploy of new
  instances) instances are closed (`io.Closer`/`Close()` when implemented).
* Deployment status: failure message names the connection and the missing secret (never a value);
  success message summarises `connections: erp-api ok`. It travels in the existing
  `DeploymentStatus.message`.
* The engine resolves a step through `engine.ConnectorLookup(flowID, stepConfig)`; the Manager
  implements it per flow. The older flat `engine.Connectors` registry stays for tests/embedding.

### CLI

`nurtuo secret set|get|list|delete` operates on a writable provider chosen with `--config` (the
runtime config) or `--provider/--path`. Values are read from stdin (piped) or a no-echo prompt and
are never accepted as an argv value. `get` needs `--reveal` to print the value.

## Consequences

Bundles stay safe to sign, store and share. Flows are portable across environments: only the secret
store differs. Deployment gives immediate, actionable errors instead of runtime 500s. Costs: the
runtime now links all native connectors; `strict` Test can block a deploy during a backend outage
(use `warn`); the keychain integration and a Windows ACL check are future work; a bridge connector
registration config and a JSON schema for the `connection` component are not part of this ADR's
implementation.
