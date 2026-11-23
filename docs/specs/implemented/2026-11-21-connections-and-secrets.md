<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Connections and secrets

Status: implemented. Date: 2026-11-21 (written as built on 2026-11-22).

## TLDR

A connection is a bundle component (`nurtuo.connection/v1`): connector id, non-secret settings and
`secret://<name>` references. At deploy time the runtime resolves the references through a secret
provider (`env`, `dir` or `encrypted-file`), creates one connector instance per (deployment,
connection), tests it, and scrubs every resolved value from errors, diagnostics and logs. The
`encrypted-file` store is AES-256-GCM, but its master key lives in a file next to it: the OS
keychain source is a stub and Windows permissions are not checked.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/25 (squash commit `ee4d733`) |
| ADRs | [ADR 0010](../../adr/0010-connections-and-secrets.md) |
| Code | `core/runtime/internal/connections/connections.go`, `core/runtime/internal/connhost/{manager.go,registry.go,redacting.go}`, `core/runtime/internal/secrets/*.go`, `core/runtime/cmd/nurtuo/secret.go` |
| User docs | [operations.md](../../operations.md), [security.md](../../security.md#secrets-handling) |

## Problem and goals

Connector steps need endpoints and credentials. Bundles are signed, stored and shared, so they
must never contain secret values, yet a deployment must fail early and clearly when a credential is
missing.

- Goals: secrets as references only; in-memory injection; per-deployment connector instances (no
  shared pools across deployments); deploy-time `Test` with a configurable policy; secret-free
  errors, status messages and logs; a CLI that never takes a secret value on argv.
- Non-goals (as built): secret rotation without redeploy, external secret managers (Vault, AWS/GCP
  /Azure), per-workspace secret scoping, OS keychain integration, Windows ACL checks.

## Design (as built)

**Connection component** (`connections.Parse`): JSON with `schema: "nurtuo.connection/v1"`, `id`
(`^[a-z][a-z0-9_-]*$`), `connector`, `settings`; unknown fields are rejected. Any settings string of
the form `secret://<name>` is a reference; `Definition.Refs()` lists them.
`connections.SecretPaths(schema)` collects the dotted paths a connector's connection schema marks
secret (`x-nurtuo-secret: true` and the equivalents listed in security.md); `CheckLiterals` fails
when such a path holds a literal. `Resolver.Resolve(ctx, def, secretPaths)` looks up every
reference, reports missing secrets by name only, adds every value to the `Redactor` and returns a
`Resolved` connection that stays in memory.

**Secret providers** (`internal/secrets`): `Provider{Get}` and `Store{Get, Set, Delete, List}`;
`Secret` renders `***` in `String`, `GoString`, `%v`, JSON and slog, only `Reveal()` yields the
value; `ErrNotFound` sentinel; names match `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}`. `secrets.Open(Config)`
picks:

| Provider | Store | Behaviour |
|---|---|---|
| `env` (default) | read-only | `secret://erp-token` reads `NURTUO_SECRET_ERP_TOKEN` (`EnvVar`: upper-case, other characters to `_`) |
| `dir` | read/write | one file per secret under `path`; on POSIX `checkPerm` refuses group/other access (0700/0600); symlinks and non-regular files refused; one trailing newline trimmed |
| `encrypted-file` | read/write | `<path>/secrets.enc`: header `NSEC1\n` (authenticated as AAD), 12-byte random nonce per write, AES-256-GCM over a JSON map, atomic replace. Key from `ChainKeySource{KeychainKeySource{}, FileKeySource{<path>/master.key or key_file}}` |

`KeychainKeySource.Key()` always returns `ErrKeychainUnavailable`, so the chain falls through to
the key file. `FileKeySource` creates a random 32-byte key with mode 0600 and `O_EXCL` on first use,
refuses a key of the wrong size or with group/other permissions (POSIX). On Windows
`perm_windows.go` makes `checkPerm` and `chmodPrivate` no-ops.

**Redaction.** `Redactor.Add` registers values of 3 bytes or more (shorter ones would shred
unrelated text), `Redact` replaces them with `***` longest first, `RedactError` drops the error
chain when the message changed, and `Redactor.Handler` wraps an `slog.Handler`. `runtime.RunWith`
wraps the runtime logger with it, and `connhost.redacting` wraps every connector handed to the
engine (errors, `Test` diagnostics, `DocumentResult.message`).

**Connector registry and instances.** `connhost.Builtins()` registers `http`, `sftp`, `postgres`,
`nats`. `Manager.Prepare(ctx, deployment, flows, defs)` checks duplicates, maps each connector step
to its connection (explicit `connection`, or the only connection for that connector; zero or
several is an error), builds only the referenced connections (resolve, create from the factory,
`Test` with `DefaultTestTimeout` 15 s under `TestMode` `strict`/`warn`/`off`) and closes everything on
error. `Commit` swaps in the prepared set and closes the previous instances; `Discard` and `Remove`
close; `Lookup(flowID, cfg)` serves `engine.ConnectorLookup`; `Describe` returns settings with
secret paths masked; `Prepared.Summary()` produces `connections: erp-api ok`.

**CLI** (`nurtuo secret set|get|list|delete <name>`): the store comes from `--provider/--path/--key-file`
(default provider `encrypted-file` when only `--path` is given), else `--config <runtime.yaml>`
(`runtime.LoadSecretsConfig`, reads only `secrets`), else the standalone store
`<data-dir>/secrets/store`. Values come from stdin or a no-echo prompt, never argv; `get` prints
the value only with `--reveal`.

## Contracts

| Contract | Stability | Notes |
|---|---|---|
| Component `nurtuo.connection/v1` (`id`, `connector`, `settings`) and kind `connection` in bundles | stable | `connections.SchemaID`, `connections.Kind`; there is no published JSON Schema for it yet |
| Reference syntax `secret://<name>` and the name pattern | stable | ADR 0010 |
| `runtime.yaml` `secrets: {provider, path, key_file}`, `connection_test: strict\|warn\|off` | stable | `runtime.Config` |
| Environment `NURTUO_SECRET_<NAME>` | stable | `env` provider |
| Files `secrets.enc` (format `NSEC1`), `master.key` (32 raw bytes) | stable on disk | changing the format needs a migration |
| CLI `nurtuo secret` actions and flags `--config`, `--provider`, `--path`, `--key-file`, `--reveal` | stable | `cmd/nurtuo/secret.go` |
| Connection schema marker `x-nurtuo-secret: true` | stable | connector authors depend on it |

## Security

- Secret values never appear in bundles, flows, status messages or `Describe` output by design;
  a literal in a schema-marked secret field fails validation.
- Redaction is exact string replacement: a secret transformed by the target (Base64 in a header
  echo, URL-encoded) is not caught, and values shorter than 3 bytes are not registered.
- `encrypted-file` protects a copied or backed-up `secrets.enc` without `master.key`; it does not
  protect against anyone who can read the data directory, because the key sits beside it.
- The CLI refuses argv values (`TestSecretCLIRefusesArgvValues`) so values do not land in shell
  history or the process list.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestSecretNeverFormats`, `TestValidName`, `TestEnvProvider`, `TestDirProvider`, `TestEncryptedStore`, `TestEncryptedTamperAndWrongKey`, `TestKeySources`, `TestRedactor`, `TestOpenConfig` in `internal/secrets/secrets_test.go` | providers, crypto, redaction | `cd core/runtime && GOWORK=off go test -race ./internal/secrets/...` | |
| `TestDirPermissionsAndSymlinks` in the same file | POSIX permission and symlink refusal | same | skipped on Windows ("POSIX permission bits are not checked on Windows"); symlink part skipped where symlinks are unavailable |
| `TestParse`, `TestResolveInjectsSecretsInMemoryOnly`, `TestResolveMissingSecretsReportedByNameOnly`, `TestProviderErrorIsNotNotFound`, `TestLiteralSecretsRejected` in `internal/connections/connections_test.go` | component parsing and resolution | `cd core/runtime && GOWORK=off go test ./internal/connections/...` | |
| `TestPrepareCommitLookupDescribeClose`, `TestDescribeMasksLiteralSecretFields`, `TestReferenceErrors`, `TestMissingSecretFailsAndClosesEarlierInstances`, `TestLiteralInSecretFieldRejected`, `TestTestModes`, `TestRedactingWrapper`, `TestBuiltins` in `internal/connhost/manager_test.go` | instance lifecycle | `cd core/runtime && GOWORK=off go test ./internal/connhost/...` | |
| `TestConfigSecretsAndConnectionTest`, `TestParseBundleConnections` in `internal/runtime/connections_test.go` | config and bundle wiring | `cd core/runtime && GOWORK=off go test ./internal/runtime/...` | |
| `TestRealWiringHTTPConnector`, `TestRealWiringTestModeWarnAndEncryptedFileSecrets` in `internal/e2e/realwiring_test.go` | end to end with real providers | `cd core/runtime && GOWORK=off go test ./internal/e2e/...` | |
| `TestSecretCLILifecycleDirProvider`, `TestSecretCLIRefusesArgvValues`, `TestSecretCLIEnvProviderIsReadOnly`, `TestSecretCLIEncryptedFileViaConfig`, `TestSecretCLIInputValidation`, `TestSecretCLIPromptIsHidden` in `cmd/nurtuo/secret_test.go` | CLI | `cd core/runtime && GOWORK=off go test ./cmd/nurtuo/...` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Standalone on Windows puts its data root in `%ProgramData%\Nurtuo` (`DefaultDataRoot`); `ProgramData` subfolders normally inherit read access for the local `Users` group, and `checkPerm`/`chmodPrivate` are no-ops on Windows, so another local user can read `secrets\store\master.key` and `secrets.enc` (and decrypt every connection secret), plus `admin-token` and `db-password`. Not verified on a real host | critical | secrets, standalone | set an explicit owner-only DACL on the secrets directory at creation (`golang.org/x/sys/windows` `SetNamedSecurityInfo`), verify it on read, and add a Windows-only test; until then document "do not run standalone on a multi-user Windows host" |
| 2 | An attacker reads a backup of the data directory; because `KeychainKeySource` is a stub, `master.key` is in the same backup and the AES-256-GCM encryption gives no protection | high | secrets | implement DPAPI / macOS Keychain / libsecret in `KeychainKeySource`; exclude `master.key` from the documented backup set |
| 3 | `master.key` is lost or corrupted (disk restore without it): every secret in `secrets.enc` is unrecoverable and every deployment fails `Prepare` with a decrypt error; there is no key export or recovery path | high | secrets | `nurtuo secret export-key`/`rekey` commands; document the key in the backup procedure as a separate secret |
| 4 | An operator rotates a database password with `nurtuo secret set`; running deployments keep the old resolved value in memory until the next redeploy, and the old value stays in the `Redactor` forever | medium | connections | `rotate`/redeploy command that re-prepares affected deployments; a `secrets.changed` reload path |
| 5 | No key rotation: the `master.key` used on day one encrypts the store forever, and a suspected key leak cannot be answered without manually exporting and re-importing every secret | medium | secrets | `nurtuo secret rekey` that re-encrypts with a new key atomically |
| 6 | Two `nurtuo secret set` processes run concurrently (a provisioning script in parallel); `Encrypted.mu` is an in-process mutex, both read the old map, and the second atomic replace silently drops the first value | medium | secrets CLI | file lock around read-modify-write (`flock` / `LockFileEx`) |
| 7 | A connector returns an error that echoes a Base64 or URL-encoded form of a secret; `Redactor` matches only the exact value, so the encoded secret reaches the status message and logs | medium | redaction | register common encodings of each value (Base64, URL, JSON-escaped) |
| 8 | A secret shorter than 3 bytes (a PIN) is never registered with the `Redactor` | low | redaction | document a minimum secret length or refuse short secrets for schema-marked fields |
| 9 | A bundle defines a connection that no flow step references; `Prepare` builds only referenced connections, so a missing secret for it is never reported and the operator learns about it only when a later flow version starts using it | low | connections | validate all defined connections' references (without `Test`) at deploy |
| 10 | `strict` connection tests block a deployment while a backend is briefly down; with several connections each test may take up to 15 s and delays commands on the gateway (see the gateway spec) | medium | connhost | test connections concurrently with a shared deadline |

## Improvement opportunities

- External secret providers behind the same `Provider` interface (HashiCorp Vault, cloud secret
  managers), configured in `secrets`.
- A JSON Schema for `nurtuo.connection/v1` in `sdk/ts/flow-schema`, so the canvas and CLI validate
  connections before signing a bundle.
- `nurtuo secret list --refs <bundle>` to show which referenced secrets are missing before deploying.

## References

- Open Mercato: `integration-secret-credential-editing`, `integrations-credentials-encryption-fail-closed`
  (refuse to start rather than run with unencrypted credentials), `encryption-key-rotation-hardening`.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
