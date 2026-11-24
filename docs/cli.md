<!-- SPDX-License-Identifier: Apache-2.0 -->
# nurtuoctl: command-line reference

`nurtuoctl` (package `@nurtuo/cli`) validates, formats, diffs, pushes and deploys flows from a terminal
or CI job. It talks to the control plane REST API through `@nurtuo/client` and validates locally with
the same `@nurtuo/flow-validate` library as the canvas and the control plane.

Every command accepts `--help`. `nurtuoctl help <command>` works too.

## Connecting

```sh
nurtuoctl login --server https://nurtuo.example.com --token-stdin < token.txt
```

`login` verifies the credentials with a request, then stores them in a JSON config file with mode
`0600` (POSIX; Windows does not enforce POSIX modes, the file lives under `%APPDATA%`).

| Setting | Flag | Environment | Config file |
| --- | --- | --- | --- |
| Server URL | `--server <url>` | `NURTUO_SERVER` | `server` |
| API token (sent as `Authorization: Bearer`) | `--token <t>` | `NURTUO_TOKEN` | `token` |

Precedence: flag, then environment, then config file. The config file is
`$NURTUO_CONFIG`, else `%APPDATA%\nurtuo\config.json` (Windows) or
`${XDG_CONFIG_HOME:-~/.config}/nurtuo/config.json`. In CI prefer `NURTUO_SERVER` and `NURTUO_TOKEN`
and never `--token` (it shows up in process listings); `login --token-stdin` keeps the token out of
shell history. The token is never printed. `nurtuoctl logout` deletes the config file.

## Output and exit codes

Human-readable text by default. With `--json` the command's result is printed to stdout as JSON
(`pull` without `-o` always prints the flow document itself). Errors go to stderr, as
`nurtuoctl: <message>` or, with `--json`, `{"error":{"code":"...","message":"..."}}`.

| Exit code | Meaning |
| --- | --- |
| 0 | success |
| 1 | failure: validation errors, API error (4xx/5xx except auth), unreadable or invalid input, deployment failed, timeout, `--check` / `--exit-code` mismatch |
| 2 | usage error: unknown command or flag, missing argument, no server configured |
| 3 | network error: connection refused, DNS failure, request timeout |
| 4 | authentication or authorization failure (HTTP 401 or 403) |

Idempotent requests (GET, PUT, DELETE) are retried twice with backoff on connection errors and on
HTTP 408/429/500/502/503/504 (honoring `Retry-After`). POST is never retried. Each attempt times out
after 30 s.

## Commands

Components are addressed by id or by name.

### Local (no server needed)

| Command | Description |
| --- | --- |
| `validate <file\|->` | Run the flow validator. Text output is `severity<TAB>code<TAB>message`. Exit 1 on errors. `--json` gives `{file, valid, diagnostics[]}`. |
| `fmt <file\|-> [-w] [--check]` | Canonical formatting: key order `schema, id, name, description, revision, steps, edges`; step keys `id, kind, label, config`; config keys sorted; edges sorted by key; steps keep authored order; 2-space indent. Prints to stdout; `-w` rewrites in place; `--check` exits 1 if the file is not canonical. |
| `diff <a> <b> [--exit-code]` | Semantic diff by step id and `step:port` edge key. Reports changed metadata, added/removed steps, per-field config changes (`config.jsonata: "x" -> "y"`) and added/removed/retargeted edges. Step order and key order are ignored. `--exit-code` exits 1 when different. |
| `init <name> [-t minimal\|http-get] [-o file] [--force]` | Scaffold a valid flow (`<id>.flow.json` by default). The id is the slugified name. |
| `completion bash\|zsh\|powershell` | Print a completion script (see below). |

### Server

| Command | Description |
| --- | --- |
| `login`, `logout` | See above. `login --no-verify` saves without contacting the server. |
| `component list` | List components. |
| `component get <c>` | Show one component. |
| `component create <name> [--kind flow\|map\|connector\|script] [-d text]` | Create. Names match `^[a-z0-9][a-z0-9._-]*$`. |
| `component delete <c>` | Delete; the server refuses (409) while the component has revisions. |
| `component revisions <c>` | List revisions (number, id, digest). |
| `flow push <file\|-> [--component c] [--create] [--force]` | Validate locally; on errors nothing is sent and the exit code is 1. Otherwise create a new immutable revision of the component named by `--component` (default: the flow's `id`). `--create` creates the component when missing. If the latest revision already has identical content (key order ignored) nothing is pushed unless `--force`. |
| `flow pull <c> [-r n] [-o file]` | Fetch a revision (latest by default); canonical JSON to stdout or to `-o`. |
| `deploy <c> --runtime <id> [-r n] [--wait]` | Record the desired state for the runtime. Revision defaults to the latest. The deployment starts `pending` and the runtime converges asynchronously. |
| `deploy status <id> [--wait] [--timeout s]` | Show a deployment. Exit 1 if `failed` or `stopped`. `--wait` polls (default 60 s, `--interval ms`) until it leaves `pending`. |
| `runtime list [-e environment]` | List runtimes. |

`deploy status` takes precedence over a component literally named `status`; deploy such a component
by id.

### Typical CI job

```sh
export NURTUO_SERVER=https://nurtuo.example.com NURTUO_TOKEN=$CI_NURTUO_TOKEN
nurtuoctl fmt flows/orders.flow.json --check
nurtuoctl validate flows/orders.flow.json
nurtuoctl flow push flows/orders.flow.json --create
nurtuoctl deploy orders --runtime rt-staging --wait
```

## `nurtuo flow run`: run a flow file locally (Go runtime binary)

`nurtuo flow run` is part of the Go binary `nurtuo`, not of `nurtuoctl`: it runs a canvas-exported
`<id>.flow.json` on the engine **in-process** with the built-in connectors (`http`, `sftp`,
`postgres`, `nats`) and prints the returned document. It needs no control plane, bundle or
deployment, opens no listener and makes no control-plane request. (`nurtuo run` is unrelated: it runs
the runtime in the foreground.) The contract is experimental.

```sh
nurtuo flow run <flow.json> [--connection <file>]... [--set key=value]... [--secrets-dir <dir>]
                [--known-hosts <file>] [--insecure-host-key] [--json] [--timeout <duration>]
                [--max-output-bytes N] [--max-documents N]
```

The optional output limits must be positive. `--max-output-bytes` bounds returned decoded payload
and temporary spill writes. JSON output also accounts for encoding and metadata. `--max-documents`
bounds the number of returned documents. An overflow fails with `output_too_large` in JSON mode
and emits no partial result.

Example: read `/downloads/version.txt` from a private SFTP server
(`examples/flows/sftp-download.flow.json`, `examples/connections/sftp.connection.json`):

```sh
export NURTUO_SECRET_SFTP_PASSWORD='...'   # or: --secrets-dir ~/.config/nurtuo/secrets
nurtuo flow run examples/flows/sftp-download.flow.json \
  --connection examples/connections/sftp.connection.json \
  --set path=/downloads/version.txt > version.txt
```

| Flag | Meaning |
| --- | --- |
| `--connection <file>` | A `nurtuo.connection/v1` file; repeatable; matched to `connector` steps by connection `id`. Secrets are `secret://<name>` references; a literal password is rejected. |
| `--set key=value` | Repeatable. Sets a property on the one document injected into `start` (the SFTP `GET` reads `path`). A `listener`/`queue` start is accepted and ignored. |
| `--secrets-dir <dir>` | Reads secrets from files (dir mode 0700, files 0600) in addition to the environment `NURTUO_SECRET_<NAME>` (checked first). Secrets are never accepted as arguments. |
| `--known-hosts <file>` | known_hosts file used to verify SFTP host keys. For `known_hosts` connections the order is: this flag, then inline `settings.knownHosts` in the connection file, then `~/.ssh/known_hosts`; an empty result is exit 1. `hostKeyPolicy: fingerprint` ignores it. |
| `--insecure-host-key` | Skips SFTP host key verification (prints a warning on stderr). The only way to skip it: a connection file with `hostKeyPolicy: insecure` and no flag exits 2 without connecting. |
| `--json` | stdout is `{"status": 200, "documents": [{"id", "properties", "payload"}], "steps": [{"id", "kind", "in", "out", "failed", "dropped"}]}` (`payload` is base64; `steps` has one entry per executed step id, counters summed, in first-seen order); stderr errors are JSON `{"error": {"code", "message"}}`, plus the same `steps` when the engine ran (failed documents, stop, timeout), and none for errors before the engine (usage, validation, connection test). Consumers must ignore unknown fields. |
| `--timeout <d>` | Bounds the whole run, for example `30s`. |

stdout is exactly the payload bytes of the single returned document (no added newline). Errors go to
stderr as `nurtuo: <message>` and stdout stays empty. A flow that returns more than one document
needs `--json`.

| Exit code | Meaning |
| --- | --- |
| 0 | success |
| 1 | execution error: connector or document failure, step kind with no runner (for example `script`, `queue`, `notify`), missing secret, host key or authentication failure, timeout or interrupt |
| 2 | usage error, invalid flow or connection file, a connection id missing from the `--connection` files, a literal secret, `hostKeyPolicy: insecure` in a connection file without `--insecure-host-key` |
| 3 | the flow ended in a Stop step with a status (`flow stopped with status <n>`) |

A flow can contain mutating operations (SFTP `DELETE`/`RENAME`, SQL writes): run only flows you
trust with real credentials.

### `nurtuo flow host-key` (experimental)

`nurtuo flow host-key <host> [--port N] [--timeout 10s] --json` prints the SHA-256 fingerprint
and type of the SSH host key negotiated by the SFTP connector defaults. The scan stops before
authentication. Compare the fingerprint with the value published by the server administrator
before trusting it.

## Shell completion

```sh
source <(nurtuoctl completion bash)        # add to ~/.bashrc
source <(nurtuoctl completion zsh)         # add to ~/.zshrc (after compinit)
nurtuoctl completion powershell | Out-String | Invoke-Expression   # add to $PROFILE
```

Scripts are generated from the command tree, so they stay in sync with the CLI. They complete command
and flag names, not component names or ids.

## The `@nurtuo/client` package

`@nurtuo/client` is a dependency-free `fetch` client for the same API.

```ts
import { NurtuoClient, ApiError } from "@nurtuo/client";

const client = new NurtuoClient({ baseUrl: "https://nurtuo.example.com", token: process.env.NURTUO_TOKEN });
const c = await client.createComponent({ name: "orders" });
const rev = await client.createRevision(c.id!, flow);
const { deployment } = await client.deploy({ componentId: c.id!, runtimeId: "rt1", revision: rev.number });
```

Methods cover components (list/get/create/update/delete), revisions (list/create/get), runtimes
(list/get/register, desired state) and deployments (list/deploy/get/reportDeploymentStatus). Failures
throw `ApiError` (`status`, `code`, `message`, `body`) for HTTP errors or `NetworkError` (`timedOut`)
when no response arrived. Options: `token`, `timeoutMs` (30000), `retries` (2), `retryDelayMs` (200),
`retryUnsafe`, `headers`, `fetch`.

The wire types in `src/schema.generated.ts` are generated from `core/control-plane/openapi.yaml`:

```sh
pnpm --filter @nurtuo/client gen          # regenerate
pnpm --filter @nurtuo/client gen:check    # fail if the committed file is stale
```

`@nurtuo/client/testing` exports `startFakeControlPlane()`, an in-memory implementation of the API
used by the client and CLI tests (it shares no code with the AGPL control plane).
