<!-- SPDX-License-Identifier: Apache-2.0 -->
# Security

A threat model sketch and an honest list of what the code verifies today. To report a
vulnerability use the private process in [SECURITY.md](../SECURITY.md); do not open public issues.

Nurtuo is early development ("Nothing here is production-ready yet", README). This page separates
**implemented and tested**, **designed but not enforced in code yet**, and **not verified**.

## Assets and actors

Assets: documents in flight (often business data), connection credentials, the signing key for
bundles, the mTLS keys, the control plane database, and the hosts the runtime runs on (inside
customer networks).

Actors: flow authors (humans, the canvas, AI agents through MCP), operators, the control plane, the
runtimes, external systems reached by connectors, and anyone who can send requests to a listener.

## Trust boundaries and threats

| Boundary | Main threats | Mitigation in code | Gap |
|---|---|---|---|
| Runtime to control plane (gateway) | eavesdropping, impersonating either side, inbound attack on the runtime | runtime dials out only and never listens for the gateway; client requires TLS 1.3 and verifies the server against a CA file; client certificate presented (mutual TLS); the control plane gateway (`core/control-plane/src/gateway.ts`) requires TLS 1.3 and a client certificate signed by its CA, takes the runtime identity from the certificate and rejects a `Hello.runtime_id` that differs | no certificate rotation or revocation; the identity is the certificate CN or DNS SAN, so issue one certificate per runtime from a CA you control |
| Deployment bundle to runtime | tampered or substituted bundle | bundle format with Ed25519 signature and hash pinning (below); with `trusted_keys` set in `runtime.yaml` the runtime accepts only signed `.bundle` archives whose digest matches the deployment and whose signature verifies against a trusted key | without `trusted_keys` the runtime still reads plain `<digest>.json` files without any signature check (the default); no key rotation or revocation list |
| Flow author to external systems | author makes a connector do more than intended | per-connector notes (below); connection secrets are `secret://` references resolved by the runtime secret store; secrets redaction | no authorization model for who may use which connection; no egress controls |
| Network client to listeners | unauthenticated calls, eavesdropping, oversized bodies | HTTP listener: opt-in `auth` on the `start` step (`bearer`, `apiKey`, or `hmac` body signatures; secrets from the secret store, constant-time comparison, `401` before the flow runs, credential header kept out of document properties); opt-in `tls` (TLS 1.2 minimum); refuses a non-loopback `http_addr` without `auth` unless `insecure: true`; body limit 1 MiB (413), method check, pause support, binds `127.0.0.1` by default; see [connectors/http.md](connectors/http.md#http-listener-start-step) | auth and TLS are off unless configured; no rate limiting, no replay protection for `hmac`, no client certificates; secrets are read at listener start (redeploy after rotation) |
| Users to the control plane REST API | unauthorised reads and writes | authentication (sessions, API tokens, optional OIDC), workspace RBAC with a fail-closed route policy, CSRF protection and an audit log; see [auth.md](auth.md) | components, runtimes and deployments are not partitioned by workspace yet (single-tenant in practice); SAML and SCIM are stubs; no TLS termination, put it behind a proxy; `NURTUO_INSECURE_NO_AUTH=1` disables everything |
| AI agent to flows | agent makes destructive changes | MCP tools only edit flows through validated commands (`apply_ops`, atomic, revision checked); `dry_run` refuses invalid flows; the `deploy` tool exists only with `--allow-deploy` and requires a second call with `confirm=true` after the human has seen the plan | the confirmation is a protocol convention the server enforces, not an authorization boundary: whoever can call the tool with the server's control plane token can deploy |
| Supply chain | malicious dependency or release artifact | license allow-lists in CI, goreleaser config with cosign keyless signing and SBOM (`.goreleaser.yaml`), committed lockfiles | release signing and CI jobs were not run for this page; the runtime does **not** verify bridge signatures (it starts whatever bridge command its configuration names) |

## mTLS between runtime and control plane

- The runtime builds client credentials from `cert_file`, `key_file` and `ca_file`
  (`core/runtime/internal/gateway/mtls.go`): TLS 1.3 minimum, server certificate verified against the
  CA file, optional `server_name` override.
- The stream is one long-lived `RuntimeGatewayService.Connect` plus `FetchArtifact`; the runtime
  always initiates.
- Standalone mode generates a local ECDSA P-256 CA (10 years), a server certificate and a client
  certificate (2 years, for `localhost`, `127.0.0.1`, `::1`), writes them under `secrets/` (private
  keys mode 0600, directory 0700; on Windows a private DACL, see "Windows: private secrets
  directory" below) and reuses them. There is no rotation or revocation.
- For distributed installs you provide the CA and issue one client certificate per runtime. The
  control plane gateway binds the identity: `Hello.runtime_id` must equal the certificate identity
  or the session is rejected (`core/control-plane/src/gateway.ts`; covered by unit and
  cross-language interop tests, not run against a production deployment).

## Bundle signing and verification

Format `nurtuo.bundle/v1` (package `core/deploy/bundle`; see
[ADR 0006](adr/0006-bundle-format-and-domain-separated-signature.md)):

- Uncompressed USTAR tar with fixed metadata, entries in a fixed order: `manifest.json`,
  `config.json`, `components/<kind>/<id>/<revision>.json`, `manifest.sig`.
- The manifest pins every other file by SHA-256 and size and lists components and connectors
  (id, version, optional registry digest), sorted.
- `manifest.sig` is `{"alg":"ed25519","keyId":"<16 hex>","sig":"<base64>"}`. The signature covers
  `"nurtuo-bundle-v1\n" + manifest.json`: a fixed domain-separation prefix, so a signature made for
  a bundle cannot be reused as a signature over other data signed with the same key. `keyId` is the
  first 16 hex characters of the SHA-256 of the public key.
- Packing is deterministic (canonical JSON, sorted collections, no clock, deterministic Ed25519).

`Verify(data, publicKey)` rejects: a missing signature, a wrong key or edited manifest, an edited
file, reordered, extra or missing entries, duplicate entries, non-regular tar entries, bytes after
the end-of-archive marker, a config path other than `config.json`, component paths that do not
match their kind, id and revision, unsorted or duplicate manifest lists, duplicate JSON object keys
(canonicalisation rejects them), and archives or contents above `MaxBundleSize` (256 MiB default,
checked before buffering). The signature is checked before anything else in the manifest is
trusted. Verified: the package's tests pass (`GOWORK=off go test ./...` in `core/deploy/bundle`,
16 test functions including the hardening tests).

What is **not** covered:

- The runtime verifies signed bundles only when `trusted_keys` is configured; with it unset, plain
  `<digest>.json` bundles are read without verification.
- Key distribution, trust store, rotation and revocation are out of scope of the package: the
  caller supplies the public key. `keyId` is informational.
- Tar header metadata (mtime, mode, owners) is not signed. Do not use the archive hash as the
  bundle's identity; identify a bundle by its verified manifest (signing key id plus manifest hash).
- `connectors[].digest` is carried in the manifest; nothing fetches or verifies connector artifacts yet.
- Private keys are hex seeds produced by `EncodePrivateKey`; storage of them is up to you (a KMS or
  file with tight permissions).

## Secrets handling

- Connection schemas mark secret fields (`x-nurtuo-secret`, `writeOnly`, `format: secret`). The
  contract says secrets are references resolved by the runtime and injected in memory
  (`ConnectionConfig`); flows must not contain secret values.
- The runtime resolves `secret://name` references in bundle connections through a secret store
  selected by `secrets.provider` in `runtime.yaml`: `env` (default, `NURTUO_SECRET_<NAME>`), `dir`
  (one file per secret) or `encrypted-file` (AES-256-GCM, master key from a key file). Manage them
  with `nurtuo secret`. Resolved values live in memory only. On POSIX the store directory and files
  must not be group/other accessible; on Windows their DACL must be private (see below). Secret
  values must not be written into flow documents or bundles.
- All four native connectors redact their configured secrets from error messages and `Test`
  diagnostics, and the contract suite can assert that for any connector (`Options.Secrets`). The
  HTTP connector masks the API key and URL password in the recorded `url`. Redaction is by exact
  string replacement; a secret transformed by the target (for example Base64 in a header echo) is
  not caught. Document payloads and properties are never scrubbed.
- Standalone secrets (CA and leaf keys, `db-password`, `admin-token`) live as files under
  `<data-dir>/secrets`; the embedded database connection uses `sslmode=disable` on loopback.

### Windows: private secrets directory

POSIX modes mean nothing on Windows, and the standalone default data root `%ProgramData%\Nurtuo`
inherits read access (and create-file access) for the local `Users` group. Nurtuo therefore uses
explicit ACLs ([spec](specs/implemented/2026-11-24-windows-secrets-dacl.md), issue #57):

- **Principals.** A private object has a protected DACL (nothing is inherited from `ProgramData`)
  that allows only the account running Nurtuo, `NT AUTHORITY\SYSTEM` and `BUILTIN\Administrators`.
  This is the equivalent of `0700`/`0600` plus root, not strictly owner-only: administrators and
  `SYSTEM` keep access, so an elevated operator can run `nurtuo secret set` and a later service
  install is not locked out. The owner must be one of those three principals too.
- **Creation.** New private directories and files get that DACL in the same call that creates them,
  so no other user can open them in between. Files written below a private directory inherit it.
- **Standalone `secrets\` is repaired.** On every start, `nurtuo standalone` (and `nurtuo secret`
  without flags, on the same store) verifies `<data-dir>\secrets` and everything below it. A
  too-open DACL, for example the inherited `Users` read of an install made before this change, is
  replaced by the private DACL, and each path is printed once to stderr:
  `standalone: tightened permissions on <path>`. Repair never changes an owner and never adds a
  principal.
- **Foreign owner: refused.** If `secrets\` or anything below it is owned by another account (for
  example a `master.key` another local user planted before the first start), standalone exits 1
  naming the path and the owner SID, without reading or changing the object. Check its contents,
  then remove it or take ownership (`takeown`). Symlinks and junctions in the tree are refused too.
- **Operator-chosen paths are never changed.** For the distributed runtime and
  `nurtuo secret --provider/--path/--key-file/--config`, an existing `dir` / `encrypted-file`
  `secrets.path` or `key_file` is only checked. If it is too open or has a foreign owner, the
  runtime refuses to start (and `nurtuo secret` exits 1) with an error that names the path, the
  offending SID, the allowed principals and the `icacls` commands to fix it, for example
  `icacls "<path>" /inheritance:r /grant:r "<account>:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"`,
  then `icacls "<path>" /remove "*<offending SID>"` (drop `(OI)(CI)` for a file). A path that
  does not exist yet is created private on the first write.
- **Every read checks.** Reading a secret, the master key or `secrets.enc` fails closed if its DACL
  was opened up after start; the error names the path and SID, never a value. Standalone repairs
  it at the next start; an operator-chosen path must be fixed by the operator.
- **Rotate on multi-user hosts.** Before this change, every local user could read the standalone
  `secrets\` (master key, `secrets.enc`, `admin-token`, `db-password`, TLS keys). On a host where
  other people or services had local accounts, treat that material as exposed: rotate the
  connection secrets, the admin token and the database password. There is no tool to rotate the
  master key yet.
- **Not covered.** The rest of the data root (`postgres\data`, `postgres\runtime`, `logs\`) keeps
  the inherited `ProgramData` ACL; that is follow-up issue
  [#78](https://github.com/nurtuo/nurtuo/issues/78). Running Nurtuo later as a different
  non-administrator account than the one that created the install fails with access denied; run it
  as that account or as an administrator.
- Do not log full bundle or connection documents.
- **Canvas Run (experimental).** The control plane keeps workspace secrets in process memory only
  (write-only API, lost on restart, single replica) and hands them to a short-lived
  `nurtuo flow run` child as `NURTUO_SECRET_<NAME>` variables in a from-scratch environment (no
  control-plane secrets, `HOME` and `TMPDIR` in a per-run 0700 temp dir). Test runs allow `sftp` with
  `GET`/`QUERY` only and refuse hosts that resolve to loopback, private, link-local or metadata
  addresses unless the operator sets `NURTUO_TEST_RUN_ALLOW_PRIVATE=1` (dangerous: lets any editor
  probe the control plane's network). DNS rebinding between the check and the child's dial is not
  prevented. `hostKeyPolicy: insecure` cannot be stored. Not supported on Windows control-plane hosts
  (503). See [the spec](specs/2026-11-23-canvas-run.md).

## Connector security notes

- **PostgreSQL: arbitrary SQL.** The SQL text comes from the input document and runs with the
  database user's privileges; there is no read-only mode. Use a least-privilege role and restrict
  who can author flows and feed documents. Full text in [connectors/postgres.md](connectors/postgres.md).
- **HTTP: SSRF.** `baseUrl` and `openApiUrl` are fetched from the runtime's network position,
  including internal addresses and metadata endpoints, and private ranges are not blocked. Only
  trusted operators should define connections; restrict egress at the network layer. Cross-host
  redirects drop configured credentials. `insecureSkipVerify` disables TLS verification
  ([connectors/http.md](connectors/http.md)).
- **SFTP: host keys.** Default policy verifies against supplied `known_hosts`; `fingerprint` pins; `insecure`
  accepts any key. Paths are confined below `rootPath` ([connectors/sftp.md](connectors/sftp.md)).
- **NATS.** Publish subject comes from document properties, so untrusted input choosing
  `nats.subject` can reach any subject the credentials allow; wildcards are rejected
  ([connectors/nats.md](connectors/nats.md)).

## Engine and expression safety

- Conditions (CEL), templates and JSONata run in-process. The CEL evaluator (`cel-go`) has no side
  effects but no cost limit is configured in the code; the basic evaluator used by the runtime
  supports a tiny grammar only. JSONata evaluation is in-process with no time limit apart from the
  caller's context.
- `script` steps run WebAssembly modules in a wazero sandbox with memory, time and output limits and a
  capability allow-list; see [scripts.md](scripts.md). Modules are not yet delivered to the runtime
  by bundles, so the sandbox is exercised by tests only.
- `processCall` nesting is capped at depth 16; `branch` is capped at 25 paths.
- The HTTP listener caps request bodies at 1 MiB; connectors cap bodies as documented per
  connector.
- The HTTP listener authenticates requests only when its `start` step sets `auth`. Without `auth`
  it starts only on a loopback `http_addr`, or with `insecure: true`.

## What is and is not verified, in short

| Claim | Status |
|---|---|
| Bundle signature, hash pinning, canonical container, size cap | implemented and unit-tested; enforced by the runtime when `trusted_keys` is set |
| Runtime mTLS client (TLS 1.3, CA verification) and gateway server (client certificates, identity check) | implemented; unit tests and cross-language interop tests, not run against a deployed control plane |
| Secret store and reference resolution (`env`, `dir`, `encrypted-file`) | implemented and tested |
| Windows private DACL for standalone `secrets\` and the `dir` / `encrypted-file` stores | implemented; unit tests on Linux, ACL tests in the non-required `go-windows` CI job; the cross-user check on a real Windows host is pending (issue #57) |
| Authentication and RBAC on the control plane REST API | implemented (sessions, tokens, OIDC, audit); SAML and SCIM are stubs |
| HTTP listener authentication (`bearer`, `apiKey`, `hmac`), TLS, refusal of an unauthenticated non-loopback bind | implemented and unit-tested (`core/runtime/internal/listener`); opt-in per flow; not run against a deployed runtime |
| WASM script sandbox | implemented (wazero) and tested; modules are not delivered by bundles yet |
| Redaction of secrets in connector errors | implemented and tested per connector |
| Release signing (cosign), SBOM | configured in `.goreleaser.yaml`, not run |
| Bridge signature verification | not implemented: the runtime does not download or verify bridges |
| MCP `deploy` tool | implemented, off by default (`--allow-deploy`), two-step confirmation |
| Penetration test or third-party audit | none |

## Hardening checklist for an early adopter

- Keep the control plane and listeners on loopback or a private network behind an authenticating proxy.
- If an HTTP listener must be reachable from other hosts, set `auth` (and `tls`, unless a proxy
  terminates TLS) on its `start` step; avoid `insecure: true`.
- Issue per-runtime client certificates from your own CA; keep keys readable by the service account only.
- Use least-privilege accounts for every connector (database role, SFTP user, NATS permissions).
- Allow egress from the runtime host only to the systems the flows need.
- Restrict who can author flows: a flow author can reach everything a connection can reach.
- Keep bundle signing keys off the runtime hosts.
