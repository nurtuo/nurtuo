<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: SFTP connector

Status: implemented. Date: 2026-11-21.

## TLDR

`connectors/sftp` is the native Go connector for files on an SFTP server: list (`QUERY`), download
(`GET`), upload (`CREATE`/`UPSERT`), delete (`DELETE`) and rename (`EXECUTE`/`RENAME`), streamed in
64 KiB chunks. The one thing to know: every path is confined below `rootPath`, **including through
server-side symlinks**, and host keys are verified by default (`known_hosts`); `insecure` exists but
must be chosen explicitly.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/29 (squash commit `d6e14a8`); https://github.com/nurtuo/nurtuo/pull/41 (open, renames the proto message `Descriptor` to `ConnectorDescriptor`, touches `Describe`) |
| ADRs | [ADR 0005](../../adr/0005-set-at-a-time-engine-with-per-document-status.md), [ADR 0010](../../adr/0010-connections-and-secrets.md) |
| Issues | none |
| Code | `connectors/sftp/` (`sftp.go`, `config.go`, `connector.yaml`, `schemas/connection.json`); registered in `core/runtime/internal/connhost/registry.go` (`Builtins`) |
| User docs | [docs/connectors/sftp.md](../../connectors/sftp.md) |

## Problem and goals

File drops over SFTP are still the most common B2B exchange (EDI, bank statements, exports). Flows
need to pick files up, deliver files atomically and move processed files away, without exposing the
rest of the server and without trusting an unverified host.

- Goals: list/get/put/delete/rename with streaming and bounded memory; atomic uploads (no partial
  file visible under the final name); `CREATE` that never overwrites; path confinement that also
  resists symlinks; password and key authentication; host-key verification by default; one result
  per document even when a stream breaks.
- Non-goals (M1): `Browse`, recursive listing, glob filters, directory create/remove operations,
  polling listener (new-file trigger), connection reuse across batches, FTP/FTPS.

## Design (as built)

**Descriptor.** `(*Connector).Describe` in `connectors/sftp/sftp.go`: id `sftp`, version `0.1.0`,
host `HOST_GO_NATIVE`, display name `SFTP`, operations `QUERY`, `GET`, `CREATE`, `UPSERT`, `DELETE`,
`EXECUTE` with custom type `RENAME`; tracked properties `path`, `size`, `truncated`;
`Capabilities{Streaming: true}`. The connection schema is `schemas/connection.json`, embedded with
`//go:embed` (`connectionSchema`), so `Describe` and the file cannot drift. `Browse` returns
`connector.ErrNotImplemented`.

**Connection settings** (`parseConfig` in `config.go`):

| Key | Default | Notes |
|---|---|---|
| `host`, `user` (required) | | |
| `port` | 22 | 1..65535 |
| `password` | | `writeOnly`; one of `password`/`privateKey` required (both may be set; both auth methods are offered) |
| `privateKey`, `passphrase` | | PEM, `writeOnly`; parsed by `ssh.ParsePrivateKey[WithPassphrase]` |
| `hostKeyPolicy` | `known_hosts` | `PolicyKnownHosts`, `PolicyFingerprint`, `PolicyInsecure` |
| `knownHosts` | | required for `known_hosts`; content staged to a temp file for `knownhosts.New` and removed |
| `hostKeyFingerprint` | | required for `fingerprint`; compared with `ssh.FingerprintSHA256` |
| `rootPath` | `/` | confinement root |
| `maxListEntries` | 10000 (`DefaultMaxListEntries`) | QUERY cap |
| `timeoutMs` | 30000 | TCP connect + SSH handshake (`dial`) |

**Session.** `dial` opens one SSH connection and SFTP subsystem per `Execute` batch (and per
`Test`), honours context cancellation through `context.AfterFunc`, and clears the deadline after
the handshake. `Test` dials and `Stat`s the root.

**Execute.** The first message must be `ExecuteStart`; `parseConfig`, `resolveOp` and `dial`
failures fail the **whole stream** with an error (redacted by `redactErr`). Documents are processed
as chunks arrive; parameters are document properties (`path`, `newPath`; for uploads alternatively
`filename` together with step option `directory`). A document whose stream switches to another
document id or ends without a `Last` chunk is aborted and reported `FAILURE` ("document
incomplete: no final chunk received").

| Operation | Behaviour |
|---|---|
| `QUERY` (`list`) | `ReadDir` of `path` (else option `directory`, else `/`); in-flight `.nurtuo-*.part` files hidden (`isTempName`); at most `maxListEntries`, then tracked `truncated=true`; output `<id>.out`, a JSON array of `{name, path, size, isDir, modTime, mode}` with property `contentType=application/json` |
| `GET` (`get`) | opens the file (a directory is `APPLICATION_ERROR`), streams it in `ChunkSize` = 64 KiB chunks as `<id>.out` with properties `path`, `filename`; tracked `size` |
| `CREATE` / `UPSERT` (`openUpload`, `finishPut`) | writes to `.nurtuo-<base>.<random>.part` (`tempName`, base capped at 64 bytes) next to the destination with `O_EXCL`, creates parent directories best effort, then renames. `CREATE` fails with `os.ErrExist` if the destination exists (checked before and after the rename, which never replaces in SFTP v3). `UPSERT` uses `rename`: `PosixRename` first, otherwise a backup-based replace that restores the old file if the move fails. Tracked `path`, `size` |
| `DELETE` | `Remove` of the confined path |
| `EXECUTE`/`RENAME` | confines both `path` and `newPath`, then `rename` (replaces the destination) |

**Path confinement.** `config.remote` joins the cleaned client path under `rootPath`;
`session.confine` then resolves both the root and the target component by component with
`Lstat`/`ReadLink` (`session.walk`, at most `maxSymlinkHops` = 40), refuses a result outside the
real root (`errOutsideRoot`) and refuses the root itself for mutating operations (`allowRoot` is
true only for `QUERY` and `GET`). Missing components are appended verbatim.

**Status mapping** (`finish`, `isApplicationError`):

| Outcome | Status |
|---|---|
| success | `SUCCESS` |
| `os.ErrNotExist`, `os.ErrPermission`, `os.ErrExist`, `errBadInput` (missing `path`/`newPath`, outside root, root itself, too many links, is a directory), SFTP status codes 2, 3, 4 | `APPLICATION_ERROR` |
| any other error (I/O, connection lost) | `FAILURE` |
| incomplete document | `FAILURE` |
| invalid settings, unsupported operation, connect/auth/host-key failure | whole stream fails |

## Contracts

| Contract | Stability |
|---|---|
| Connector id `sftp`, version `0.1.0`, the six operations above | experimental |
| Connection keys in `schemas/connection.json` | experimental |
| Document properties `path`, `newPath`, `filename`; step option `directory` | experimental |
| Output document `<id>.out` (listing JSON shape; download properties `path`, `filename`) | experimental |
| Temp-file naming `.nurtuo-<base>.<hex>.part`, backup naming `<dest>.nurtuo-bak-<hex>` | internal (visible on the server) |
| Tracked properties `path`, `size`, `truncated` | experimental |

## Compatibility

No contract change in this spec. Open https://github.com/nurtuo/nurtuo/pull/41 changes the return
type of `Describe` to `*connectorv1.ConnectorDescriptor`; wire format unchanged, Go type name
changes. Update this spec when it merges.

## Security

- **Host keys.** Default `known_hosts`; `fingerprint` pins one SHA-256 fingerprint; `insecure`
  uses `ssh.InsecureIgnoreHostKey` and must be configured explicitly. `known_hosts` and
  `fingerprint` require their value; a missing value is a configuration error, never a silent
  fallback.
- **Confinement.** Paths, `..`, backslashes and symlinks cannot leave `rootPath`
  (`TestSymlinkEscapeAndRootProtection`, `TestConfinementBelowRootPath`). The check-then-use window
  is documented in `confine` (SFTP has no `openat`).
- **Secrets.** `password`, `privateKey`, `passphrase` are `writeOnly` (recognised by
  `connections.SecretPaths`); `redact` masks them in results, diagnostics and stream errors; the
  runtime `redacting` wrapper is a second layer. Key parse errors use a fixed message.
- **Atomicity.** No partially written file appears under the destination name; aborted uploads
  remove their `.part` file.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestContract` in `connectors/sftp/sftp_test.go` | shared suite: Describe, Test redaction, Execute cases `put` (incl. 1 MiB + 17 bytes and missing path), `get` (incl. missing file), `list`, `rename`, `delete` (second delete is `APPLICATION_ERROR`) | `cd connectors && GOWORK=off go test -race ./sftp/...` | in-process SSH server (`startServer`, `pkgsftp.InMemHandler`), policy `fingerprint` |
| `TestHostKeyAndTest` | Test ok, wrong password not leaked, fingerprint mismatch fails | same | |
| `TestRoundTripContent` | 3 MiB + 5 bytes round trip in several chunks, listing, `../../etc/passwd` rejected | same | |
| `TestUnfinishedUploadSwitchedAwayIsAbortedAndFailed`, `TestUnfinishedUploadAtStreamEnd` in `hardening_test.go` | one `FAILURE` per incomplete document, `.part` removed | same | |
| `TestUpsertFailedRenameKeepsExistingDestination`, `TestUpsertFallbackReplacesDestination` | backup-based replace without `posix-rename` (fault injection via `noPosix`) | same | |
| `TestSymlinkEscapeAndRootProtection`, `TestConfinementBelowRootPath` | symlink escapes and root protection | same | |
| `TestCreateFailsIfExistsUpsertOverwrites` | `CREATE` vs `UPSERT` | same | |
| `TestListExcludesPartFilesAndCaps` | `.part` hidden, `maxListEntries`, `truncated` | same | |
| `TestUploadUsesPartFileThenRenames` | temp-then-rename | same | |
| `TestDialHonoursContextCancellation` | cancellation during connect/handshake | same | |
| `TestSecretsNeverLeakInResults`, `TestTrackedPathValues` | redaction, tracked values | same | |

Gaps in coverage: no test for `known_hosts` or `privateKey` authentication (all tests use
`fingerprint` + password), nothing runs against OpenSSH or a commercial SFTP server, and the
contract-suite subtests `ExecuteLargePayload`, `CrashBehaviour`, `Listen` are skipped.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The server stalls mid-transfer (full disk on a NAS, half-open TCP). `timeoutMs` only bounds connect + handshake (`dial` clears the deadline), there is no SSH keepalive, so the batch hangs until the runtime cancels the stream context; with no step deadline it hangs indefinitely. | medium | reliability | Per-operation deadlines (wrap reads/writes), SSH keepalive requests, an `operationTimeoutMs` setting. |
| 2 | An inbound directory holds 2 million files. `list` calls `ReadDir`, which loads every entry into memory before `maxListEntries` is applied, and then returns only the first 10000 with no way to fetch the rest. | medium | reliability, usability | Read directories incrementally (`ReadDirContext`/paged), add glob/`modifiedSince` filters and a continuation token. |
| 3 | An upload fails with SFTP status 4 (`SSH_FX_FAILURE`), which many servers use for quota exceeded, disk full or a locked file. `isApplicationError` maps code 4 to `APPLICATION_ERROR`, so the document goes to the error path and is not retried although the condition is transient. | medium | status mapping | Map code 4 to `FAILURE` (or make it configurable); keep 2 and 3 as `APPLICATION_ERROR`. |
| 4 | The operator pastes a `known_hosts` line with only an `ssh-rsa` key; the server offers `ssh-ed25519` first. `ssh.ClientConfig.HostKeyAlgorithms` is not derived from the known hosts, so the handshake fails with a key mismatch; a non-22 port also needs the `[host]:port` form. Operators "fix" it with `hostKeyPolicy: insecure`. | medium | security, usability | Set `HostKeyAlgorithms` from the matching known_hosts entries; explain the `[host]:port` form in the Test diagnostic. |
| 5 | The SFTP host key is rotated; with `fingerprint` (one value) every batch fails until someone edits the connection. | low | operations | Accept a list of fingerprints and a known_hosts with several keys (see Improvement opportunities). |
| 6 | An invalid setting or an auth failure fails the whole `Execute` stream with an error instead of one `FAILURE` per document (the HTTP connector does the latter). Behaviour depends on how the engine treats a stream error. | low | contract consistency | Decide one rule in the connector contract spec and align connectors. |
| 7 | A `GET` stream breaks after some 64 KiB chunks were sent: the result is `FAILURE`, but output chunks of `<id>.out` without `Last` were already emitted and are not listed in `OutputDocumentIds`. No test checks that the engine (`core/runtime/internal/engine/steps.go`) drops such unlisted partial outputs. | low | contract | Cover in the contract suite (`CrashBehaviour`). |
| 8 | A server user with write access swaps a directory for a symlink between `confine` and `Open` (documented check-then-use window) and reads a file outside `rootPath`. | low | security | Use a chrooted SFTP account on the server; documented in code. |
| 9 | `docs/connectors/sftp.md` says symlinks are not resolved and that `CREATE` and `UPSERT` are identical, and omits `maxListEntries` and tracked `truncated`; `schemas/connection.json` uses `oneOf` for `password`/`privateKey`, which rejects a connection that sets both although `sshConfig` supports it. | low | docs | Update the user doc and change `oneOf` to `anyOf`. |
| 10 | `CREATE` relies on rename refusing to overwrite; a server that implements rename as replace (some Windows-based servers) lets a concurrent writer's file be overwritten between the `Lstat` check and the rename. | low | correctness | Test against common servers; prefer the `hardlink@openssh.com` extension when available. |

## Improvement opportunities

- **Key rotation**: several host-key fingerprints, several known_hosts keys, and client key
  rotation (two `privateKey` values tried in order) so rotations need no downtime.
- **New-file trigger** (a polling `Listen`) with move-after-processing and duplicate protection
  (state by name + size + mtime).
- Glob filters, recursive listing, `modifiedSince`, sorted output and pagination for `QUERY`.
- `MKDIR`/`RMDIR` and `COPY` custom operations; `GET` with `deleteAfter` or `moveTo`.
- Connection reuse across batches (pool keyed by settings hash, like the Postgres connector).
- Checksums (`check-file` extension or SHA-256 while streaming) as a tracked property.
- Certificate-based host keys (`@cert-authority`) and SSH certificates for client auth.

## References

- Open Mercato `storage-hub`: storage abstraction and file lifecycle; ideas for move-after-read,
  retention and checksums.

## Changelog

- 2026-11-22: created (as built, from `main` at `3d3d506`).
