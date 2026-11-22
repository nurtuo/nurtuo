<!-- SPDX-License-Identifier: Apache-2.0 -->
# SFTP connector (`sftp`)

Native Go connector using `github.com/pkg/sftp` over `golang.org/x/crypto/ssh`. Source:
`connectors/sftp`, descriptor `connectors/sftp/connector.yaml`, connection schema
`connectors/sftp/schemas/connection.json`. Version `0.1.0`, host `go-native`, streaming
capability.

Status: tested against an in-process SFTP server in the connector tests and registered in the `nurtuo` binary, so a deployed flow can use it through a bundle connection (secrets resolved from the runtime secret store, see [operations.md](../operations.md)). Not run against a production deployment.

## Operations

| Operation | What it does | Document properties |
|---|---|---|
| `QUERY` | lists a directory; output document `<id>.out` is a JSON array of entries `{name, path, size, isDir, modTime, mode}` | `path` (else step option `directory`, else `/`) |
| `GET` | downloads a file, streamed in 64 KiB chunks, output `<id>.out` with properties `path`, `filename` | `path` |
| `CREATE`, `UPSERT` | uploads the payload; identical behaviour, an existing file is replaced | `path`, or `filename` together with step option `directory` |
| `DELETE` | removes a file | `path` |
| `EXECUTE` with custom type `RENAME` | renames or moves a file, replacing the destination | `path`, `newPath` |

Uploads are written to `.nurtuo-<name>.part` next to the destination and then renamed (the
atomic `posix-rename` extension is used when the server supports it, otherwise remove + rename).
Missing parent directories are created best effort. A failed or aborted upload removes the
`.part` file. `Browse` is not supported.

## Connection settings

| Setting | Type | Default | Notes |
|---|---|---|---|
| `host` | string (required) | | |
| `port` | integer 1..65535 | 22 | |
| `user` | string (required) | | |
| `password` | string | | one of `password` or `privateKey` is required |
| `privateKey` | string | | PEM; `passphrase` for an encrypted key |
| `passphrase` | string | | |
| `hostKeyPolicy` | `known_hosts`, `fingerprint`, `insecure` | `known_hosts` | see security notes |
| `knownHosts` | string | | content of a known_hosts file; required for `known_hosts` |
| `hostKeyFingerprint` | string | | `SHA256:...`; required for `fingerprint` |
| `rootPath` | string | `/` | every remote path is confined below it |
| `timeoutMs` | integer >= 1 | 30000 | SSH connect timeout |

A connection is opened per Execute batch and closed at the end.

## Tracked properties

`path` and `size` (exposed as `tracked.path`, `tracked.size`).

## Status mapping

| Outcome | Status |
|---|---|
| success | `SUCCESS` |
| file or directory missing, permission denied, already exists, SFTP status codes 2, 3, 4, missing `path`/`newPath`, path is a directory | `APPLICATION_ERROR` |
| anything else (I/O, connection lost) | `FAILURE` |
| invalid settings, unsupported operation, connect or auth failure | the whole stream fails with an error |

## Security notes

- **Host keys.** The default policy `known_hosts` verifies against the `knownHosts` content you
  supply. `fingerprint` pins one SHA-256 fingerprint. `insecure` accepts any key (man-in-the-middle
  exposure); it must be chosen explicitly and is not meant for production.
- **Path confinement.** Remote paths are cleaned and joined under `rootPath`, so `..` cannot
  escape the root as seen by this connector. Server-side symlinks are not resolved or blocked by
  the connector; configure the SFTP server account accordingly.
- Secrets (`password`, `privateKey`, `passphrase`) are redacted from error messages. The
  `knownHosts` content is briefly written to a temporary file because `x/crypto` only parses
  known_hosts from files; the file is removed immediately.
- Use a dedicated least-privilege SFTP account.

## Run it locally

`nurtuo flow run` runs a flow with this connector without a control plane: the path of a `GET` is
the document property `path`, set with `--set path=/downloads/version.txt`. Host keys are checked
against `~/.ssh/known_hosts` (`--known-hosts <file>`); `--insecure-host-key` skips the check and warns.
Put the password or private key in `NURTUO_SECRET_<NAME>` or `--secrets-dir`, never in the connection
file. See [cli.md](../cli.md#nurtuo-flow-run-run-a-flow-file-locally-go-runtime-binary) and
`examples/flows/sftp-download.flow.json`.

## Limits

No size limit is enforced by the connector (payloads and downloads are streamed with bounded
memory); the practical limits are the server's and the document spill settings of the runtime.
