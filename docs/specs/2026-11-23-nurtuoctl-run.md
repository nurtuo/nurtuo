<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: `nurtuo flow run` (run a canvas-exported flow locally against connection files)

Status: approved. Date: 2026-11-23.

Extends: [connector-sftp](implemented/2026-11-21-connector-sftp.md),
[engine-interpreter](implemented/2026-11-21-engine-interpreter.md),
[ts-client-and-cli](implemented/2026-11-21-ts-client-and-cli.md).

> The file name keeps the draft's `nurtuoctl-run` slug; the command is **`nurtuo flow run`** (Decision 1).
> Items marked **[ASSUMPTION]** are reversible drafting details that were not part of the approved
> decisions; they are non-blocking and may be changed in the implementation PR with a changelog line.

## TLDR

`nurtuo flow run <flow.json> --connection <file> [--set k=v]` runs a canvas-exported flow on the
engine **in-process** with the built-in connectors (`http`, `sftp`, `postgres`, `nats`) and prints the
returned document to stdout. No control plane, no deploy. Motivating case: read `/downloads/version.txt`
from a private SFTP server. The command lives in the Go binary `nurtuo` (`core/runtime/cmd/nurtuo`),
because the engine is `internal` AGPL Go code and the TypeScript `nurtuoctl` cannot link it. The
existing `nurtuo run` (runtime in the foreground) is unchanged.

## Goal

A developer with a flow file and a connection file runs one command and sees the output document of
the flow on stdout, executed by the real engine and a real built-in connector, without a control
plane, bundle or deployment.

### Motivating example

```sh
export NURTUO_SECRET_SFTP_PASSWORD='...'   # or: --secrets-dir ~/.config/nurtuo/secrets
nurtuo flow run sftp-download.flow.json \
  --connection private-sftp.connection.json \
  --set path=/downloads/version.txt > version.txt
```

`private-sftp.connection.json` (kind `nurtuo.connection/v1`, no literal secret):

```json
{ "schema": "nurtuo.connection/v1", "id": "private-sftp", "connector": "sftp",
  "settings": { "host": "sftp.example.com", "port": 22, "user": "deploy",
                "password": "secret://sftp_password" } }
```

The flow is `start` (manual, or a listener/http start that is ignored) -> `connector` (`sftp`, `GET`,
connection `private-sftp`) -> `returnDocuments`. The path arrives as the initial document property
`path` set by `--set`. The host key is checked against `~/.ssh/known_hosts` (unless the connection uses a `fingerprint` policy).

## Decisions

Dated 2026-11-23, approved by Łukasz (Europe/Warsaw). These supersede the draft's Open Questions; no
blocking question remains.

| Q | Topic | Decision |
|---|---|---|
| Q1 | Command | `nurtuo flow run <flow.json>` in the Go binary. |
| Q2 | Where the code lives | Go, in `core/runtime/cmd/nurtuo`; engine in-process. |
| Q3 | Connection files | `--connection <file>`, repeatable, matched by connection `id`. |
| Q4 | Connection kind | `nurtuo.connection/v1` unchanged; secrets as `secret://`; no literal passwords. |
| Q5 | SFTP credentials | Password and key both supported; secrets from env `NURTUO_SECRET_*` or `--secrets-dir`. |
| Q6 | Host key | Verified by default; disabled only with `--insecure-host-key`, with a warning on stderr. A file-level `hostKeyPolicy: insecure` is not an opt-out (exit 2). Precedence: see Design, step 4. |
| Q7 | stdout | Raw document bytes; `--json` prints the envelope `{status, documents}`. |
| Q8 | Exit codes | 0 ok, 1 execution error, 2 usage/flow validation error, 3 Stop with status. |
| Q9 | Errors | On stderr, stdout empty; `--json` switches the stderr format. |
| Q10 | Connectors | All builtin connectors (`http`, `sftp`, `postgres`, `nats`); no bridges. |
| Q11 | Listener/http start | Accepted and ignored; one empty document is injected. |
| Q12 | Path | `--set path=/downloads/version.txt` (initial document property; `--set` is a repeatable `key=value`). |
| Q13 | Splitting | One spec; not split. |
| Q14 | Test infrastructure | Container-based SFTP e2e in CI, gated by an env var like the Postgres tests (`NURTUO_TEST_POSTGRES_URL`). |

Open questions: none blocking. Non-blocking drafting assumptions are marked **[ASSUMPTION]** below and
listed under Gaps and risks.

## Scope

In scope:

- Subcommand `nurtuo flow run` that loads a flow file and one or more connection files, validates
  them, builds the connector registry from `connhost.Builtins()`, runs the engine in-process and prints
  the returned document(s).
- `http`, `sftp`, `postgres`, `nats` as usable connectors; SFTP is the one exercised end to end.
- Local secret handling (env, `--secrets-dir`) and SFTP host-key verification.
- Flags, exit codes and stdout/stderr contract; unit tests and one container e2e.

Non-goals:

- Deploying, pushing or talking to the control plane; no bundle, signing or gateway.
- Serving listeners or triggers: the `start` step's `type`/`path` is ignored; no HTTP listener, queue or cron.
- Bridge connectors; `queue`/`notify` step kinds (no runner on `main`).
- Running more than once, watching files, interactive secret prompts.
- Changing `nurtuo.flow/v1`, `nurtuo.connection/v1`, the SFTP connector or the existing `nurtuo run`.
- A TypeScript `nurtuoctl run` wrapper (not part of this spec).

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/50 (this spec); implementation PRs after merge |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) |
| Issues | none |
| Code (existing) | `core/runtime/cmd/nurtuo/`, `core/runtime/internal/{engine,connections,connhost,secrets}/`, `connectors/sftp/`, `sdk/go/flow/` |
| User docs | [cli.md](../cli.md), [connectors/sftp.md](../connectors/sftp.md), [getting-started.md](../getting-started.md) |

## Design

### What exists today

- **`nurtuoctl`** is `@nurtuo/cli` (`cli/package.json`, bin `nurtuoctl`, TypeScript, Apache-2.0).
  Sources in `cli/src/` (`cli.ts`, `flowtools.ts`, `args.ts`, `config.ts`). Local commands are
  `validate`, `fmt`, `diff`, `init`, `completion`; the rest talk to the control plane
  ([cli.md](../cli.md)). There is **no `run` command**. The CLI depends on `@nurtuo/flow-schema`,
  `@nurtuo/flow-validate`, `@nurtuo/client`; `scripts/check-boundaries.sh` fails if an Apache
  package depends on AGPL `core/`.
- **`nurtuo`** is the Go runtime binary (`core/runtime/cmd/nurtuo`, AGPL-3.0). Commands registered in
  `main.go`: `run`, `standalone`, `service`, `secret`, `version`; `role` is a stub. **`nurtuo run`
  already exists** (`run.go`, `runMain`): it runs the runtime in the foreground from
  `--config`, not a single flow file. Subcommands self-register via `register(name, fn)` with the
  signature `func(args []string, stdout, stderr io.Writer) int`, exit codes 0/1/2.
- **Engine**: `engine.New()` (`core/runtime/internal/engine/interp.go`) returns an `Engine` whose
  `Run(ctx, *flow.Flow, *docs.Set, *Process) (*Result, error)` interprets a flow set-at-a-time.
  `Result.Returned` holds documents that reached `returnDocuments`, `Result.Failed` errored
  documents, `Result.Stop` a Stop step. `NewRegistry` registers only `message`, `setProperties`,
  `map`; the runtime adds `start`, `script` and `connector` (`core/runtime/internal/runtime/run.go`,
  `eng.Registry.RegisterConnectorLookup(mgr)`). A local runner would have to register `start` and
  `connector` itself. Engine packages are `internal`, so only code inside `core/runtime/` can import them.
- **Connections**: `connections.Parse` (`core/runtime/internal/connections/connections.go`) parses
  `nurtuo.connection/v1` (`schema`, `id` matching `^[a-z][a-z0-9_-]*$`, `connector`, `settings`;
  unknown fields rejected). `Resolver.Resolve` replaces `secret://<name>` references in memory from a
  `secrets.Provider` (`env`: `NURTUO_SECRET_<NAME>`, `dir`, `encrypted-file`).
  `(*connections.Definition).CheckLiterals`, called from `Resolver.Resolve`, rejects literal values in secret-marked fields (`writeOnly`).
  `connhost.Manager.Prepare` validates, resolves, builds and `Test`s connections for a deployment and
  `Manager.Lookup` serves them to the `connector` step.
- **Connector registry**: `connhost.Builtins()` (`core/runtime/internal/connhost/registry.go`)
  registers `http`, `sftp`, `postgres`, `nats`; `cmd/nurtuo/run.go` exposes it as `nativeConnectors()`.
- **SFTP connector**: `connectors/sftp` (`sftp.go`, `config.go`, `schemas/connection.json`; Apache-2.0).
  `GET` streams a file as output document `<id>.out` (properties `path`, `filename`; tracked `size`).
  The path comes from the input document property `path`. `directory` is an `Execute` option (for uploads) that the `connhost.Manager.Lookup` entry leaves empty, and `flow.ConnectorConfig` has no options field, so a flow cannot set it; the path for `GET` is passed with a `setProperties` step or `--set`.
  Settings: `host`, `port`, `user`, `password`, `privateKey`, `passphrase`, `hostKeyPolicy`
  (`known_hosts` default, `fingerprint`, `insecure`), `knownHosts` (**content**, not a path),
  `hostKeyFingerprint`, `rootPath`, `timeoutMs`. Secrets are `writeOnly`.
- **Flow schema**: `nurtuo.flow/v1` (`sdk/go/flow/flow.go`, `flow.v1.schema.json`; Go entry points
  `flow.Parse`, `flow.ValidateJSON`). `ConnectorConfig` is `{connector, operation, objectType?, connection?}`:
  **it has no `path`/`options` field**, so a GET path must be set earlier with a `setProperties`
  step (`PropertyAssignment{name, scope, value}`). Canvas downloads `<id>.flow.json` and
  `<id>.layout.json` (canvas-editor spec; `core/web/src/model/flowAdapter.ts`).
- **Examples**: `examples/flows/get-employee.flow.json` (HTTP listener start, Postgres connector, no SFTP);
  `examples/flows/reference/connector.flow.json` (start `manual`, HTTP connector `GET`, connection `orders-api`).
  **There is no SFTP or download example today.**

### Drift found while designing

| Path | What the doc says | What the code does |
|---|---|---|
| `connectors/sftp/connector.yaml` (UPSERT description) | "Same as CREATE (overwrites)" | `CREATE` never overwrites; `UPSERT` does (`openUpload`/`finishPut` in `connectors/sftp/sftp.go`; the SFTP spec says so). Not on the path of this feature; recorded only. |
| [docs/connectors/sftp.md](../connectors/sftp.md) | symlinks not resolved, CREATE == UPSERT (per the SFTP spec's gap 9) | `session.confine` resolves symlinks. Not on this path. Recorded only. |
| `connectors/sftp/schemas/connection.json` | password or key, "both allowed" in `sshConfig` | `oneOf` rejects both set (SFTP spec gap 9). On this path only as "password or key" (Decision 5): the command never sets both on one connection; the `oneOf` is left as is. |

No existing spec describes a local "run a flow file" command, so there is no spec-vs-code drift for the
command itself. `nurtuo run` (runtime in the foreground) is untouched by this spec.

### The command

A new subcommand registered through `register("flow", ...)` in `core/runtime/cmd/nurtuo` (new files
`flow.go`, `flowrun.go`). `nurtuo flow` dispatches on its first argument; only `run` exists, other
values exit 2 with usage. Signature is the existing `func(args []string, stdout, stderr io.Writer) int`.
`nurtuo run` is not touched, so there is no name conflict.

Steps of `nurtuo flow run <flow.json> [flags]`:

1. Parse args (see Contracts). Unknown flag, missing `<flow.json>`, malformed `--set`: exit 2.
2. Read the flow; `flow.ValidateJSON` then `flow.Parse`. Failure: exit 2, diagnostics on stderr, no connection built.
3. Read each `--connection` file with `connections.Parse`; reject duplicate ids. For every `connector` step, look up the connection by `id`; a missing id or a `connector` kind that differs from the step's is exit 2 before any network access (same rule as `connhost.Manager.Prepare`).
4. Apply host-key rules to `sftp` connections in memory, in this order (the connector's `parseConfig` only defaults an empty policy, so the CLI defines the precedence). Connection files on disk are never modified.
   1. `--insecure-host-key` sets `hostKeyPolicy: insecure` and prints a warning on stderr.
   2. A file that says `hostKeyPolicy: insecure` without the flag exits 2 and does not connect.
   3. `hostKeyPolicy: fingerprint` keeps `hostKeyFingerprint`; it does not need or read `--known-hosts`.
   4. For `known_hosts` (the default), an explicit `--known-hosts <file>` wins and fills `settings.knownHosts`; otherwise a non-empty `settings.knownHosts` is kept; otherwise `~/.ssh/known_hosts` is read. Exit 1 only if the resulting content is still empty (missing or empty file), with a message suggesting `--known-hosts`.
5. Secrets: `connections.Resolver` with the `env` provider (`NURTUO_SECRET_<NAME>`) and, when `--secrets-dir` is set, the `dir` provider **[ASSUMPTION: env first, then dir]**. `Redactor` is active for all stderr output. A missing secret names the secret, never a value. `CheckLiterals` runs inside `Prepare`, so the implementation must classify that error (a literal in a secret field -> exit 2; unavailable secret, `Test`/auth/host-key failure -> exit 1) rather than mapping every `Prepare` error to 1.
6. `connhost.NewManager(connhost.Builtins(), resolver)`; `Prepare` + `Commit` for a synthetic deployment, which `Test`s each connection (fail fast on auth or host-key error); `defer Close`.
7. `engine.New()`; register `connector` (`RegisterConnectorLookup(mgr)`); `map`, `setProperties`, `message` come from `NewRegistry`. Registering `start` is optional: an unregistered `start` step already passes the set through (`interp.go` `runStep`), and `listener.StartRunner` only validates the config. `decision`, `route`, `branch`, `tryCatch`, `stop` and `returnDocuments` are interpreter cases (`interp.go`), not registry runners. Registry kinds without a runner (`script`, `queue`, `notify`) fail with the engine's `no_runner`; no script runner is registered (`RegisterScriptRunner` is not called). A `start` of type `listener`/`http` is accepted and ignored (Decision 11).
8. Inject **one empty document** carrying the `--set` properties into `start` and `Run` under a signal-aware context (`--timeout` bounds the whole run **[ASSUMPTION: flag, default none]**).
9. Report: success prints per the output format; `Result.Failed` -> exit 1; `Result.Stop` with status -> exit 3; close all sets and connections.

### Credentials (Decision 5)

SFTP password auth: `settings.password = "secret://sftp_password"`. Key auth:
`settings.privateKey = "secret://sftp_key"` and optionally `settings.passphrase = "secret://sftp_passphrase"`. A key is a multi-line
PEM and fits a `--secrets-dir` file better than env. Both are supported; one connection uses one of them,
because the connector schema `oneOf` rejects both on the same connection (unchanged). No secret is an
argv value.

### Output format (Decisions 7-9)

- Default: stdout is exactly the payload bytes of the single returned document, no added newline. Stdout is
  written only on success; on any error stdout is empty and the error is on stderr as `nurtuo: <message>`.
- `--json`: stdout is the envelope `{"status": <int>, "documents": [{"id", "properties", "payload"}]}`
  **[ASSUMPTION: `payload` is a base64 string, `status` is 200]**. A Stop never prints the envelope. Errors with `--json`
  go to stderr as one JSON object `{"error": {"code", "message"}}` and stdout stays empty.
- More than one returned document without `--json` fails with exit 1 and tells the user to use `--json`
  **[ASSUMPTION]**. Zero returned documents succeeds with empty stdout.
- Stop with status (Decision 8): exit 3, stderr `nurtuo: flow stopped with status <n>`; with `--json` stderr carries the JSON error. A Stop never prints the envelope; stdout stays empty **[ASSUMPTION]**.
- Logs and diagnostics go to stderr only (`slog` text handler as in `run.go`).

## Contracts (all **experimental**)

| Contract | Definition |
|---|---|
| Command | `nurtuo flow run <flow.json> [--connection <file>]... [--set key=value]... [--secrets-dir <dir>] [--known-hosts <file>] [--insecure-host-key] [--json] [--timeout <duration>]` |
| `--connection <file>` | repeatable; each file is one `nurtuo.connection/v1` document; matched to `connector` steps by connection `id`; required when the flow has a `connector` step |
| `--set key=value` | repeatable; sets an initial document property on the injected document (`path=/downloads/version.txt`); first `=` splits; duplicate key: last wins **[ASSUMPTION]** |
| `--secrets-dir <dir>` | `dir` secrets provider (files 0600, dir 0700, per the existing provider); in addition to env |
| `--known-hosts <file>` | known_hosts file whose content fills `settings.knownHosts` of `sftp` connections with policy `known_hosts`; wins over inline `knownHosts`; ignored for `fingerprint`; without it, non-empty inline `knownHosts` is kept, else `~/.ssh/known_hosts` is read |
| `--insecure-host-key` | sets `hostKeyPolicy: insecure` for `sftp` connections; prints a warning on stderr; the only way to skip verification (a file saying `insecure` without the flag exits 2) |
| `--json` | stdout envelope `{status, documents}`; stderr errors as JSON |
| `--timeout <d>` | bounds the whole run **[ASSUMPTION]** |
| Flow input | `nurtuo.flow/v1` as exported by the canvas (`<id>.flow.json`); `.layout.json` not needed |
| Connection input | `nurtuo.connection/v1` unchanged; secrets as `secret://<name>`, literals in secret fields rejected by `(*connections.Definition).CheckLiterals` |
| Environment | `NURTUO_SECRET_<NAME>` |
| stdout | raw payload bytes (default) or the JSON envelope (`--json`); empty on error |
| stderr | `nurtuo: <message>` text or JSON error (`--json`); warnings, diagnostics; never secrets |
| Exit codes | 0 ok; 1 execution error (connector, `Result.Failed`, `no_runner`, cancel, missing secret, empty known_hosts, host-key/auth failure); 2 usage, flow/connection validation error, literal secret, file-level `insecure` without the flag; 3 Stop with status |

Exit codes differ from the `nurtuoctl` table (0/1/2/3 network/4 auth) on purpose (Decision 8); they are
documented in `cli.md` by the implementation PR.

## Compatibility

No existing contract changes: `nurtuo.flow/v1`, `nurtuo.connection/v1`, the SFTP connector and the
runtime config are used as they are. `nurtuo run [--config path]` and its exit codes are unchanged;
`flow` is a new top-level subcommand name in the Go binary, so nothing breaks. The `nurtuoctl`
TypeScript CLI gains nothing, and `scripts/check-boundaries.sh` is unaffected because no Apache package
imports `core/`.

## Security

- **Secrets never in files.** Connection files hold `secret://` references; the resolver injects the value in memory (ADR 0010). Literal passwords are rejected (exit 2) by `(*connections.Definition).CheckLiterals` and the error names the `secret://` fix and the `NURTUO_SECRET_<NAME>` variable. `Secret` renders `***`; `Redactor` scrubs errors and logs. Output document content is **not** redacted (user data), so the command never sends stdout to logs.
- **Secret sources.** Env `NURTUO_SECRET_*` (visible to same-user processes via `/proc/<pid>/environ`) or `--secrets-dir` (0700/0600 enforced by the provider). Keys (multi-line PEM) fit `--secrets-dir`. No secret is accepted as an argv value (same rule as `nurtuo secret`).
- **Host-key verification (Decision 6).** On by default. `--insecure-host-key` is the only opt-out; it always prints a warning on stderr and is never implied by any config. A file-level `hostKeyPolicy: insecure` without the flag exits 2 and does not connect. `fingerprint` keeps `hostKeyFingerprint` and does not read `--known-hosts`. For `known_hosts`: explicit `--known-hosts` wins, else non-empty inline `settings.knownHosts`, else `~/.ssh/known_hosts`; still-empty content exits 1 with a message suggesting `--known-hosts`.
- **Path confinement.** `rootPath` confinement of `connectors/sftp` applies; a flow can read any file under `rootPath`.
- **Untrusted flow.** A flow can contain mutating `connector` operations (SFTP `DELETE`/`RENAME`, SQL writes) and `map` expressions; running a foreign flow with real credentials is the user's risk (see Gaps). `script` steps fail with `no_runner` (no script runner is registered). No listener is opened and no control-plane request is made.
- **Output to terminal.** Writing a binary file to a TTY can inject escape sequences; refusing non-UTF-8 output to a TTY unless `--json` is a non-blocking **[ASSUMPTION]**.
- **Resource limits.** Large payloads stream through `docs.Spool` (spills to disk); stdout is written with `io.Copy`, `EPIPE` is handled and the temp dir is cleaned up.

## Step plan (for the implementation PRs, **not in this PR**)

Phases are independently shippable. Each step leaves the system working and names its verification.

### Phase 1: command skeleton, parse and validate (no network)

1. `flow` subcommand with `run` dispatch, usage text, flag parsing (`--connection`, `--set`, `--secrets-dir`, `--known-hosts`, `--insecure-host-key`, `--json`, `--timeout`); bad usage exits 2. Verified by `TestFlowRunUsage`; `nurtuo run` unchanged, verified by the existing `run` tests.
2. Load and validate the flow with `flow.ValidateJSON`/`flow.Parse`; diagnostics on stderr, exit 2. Verified by `TestFlowRunInvalidFlow`.
3. Load connection files with `connections.Parse`; cross-check `connector` steps by `id`. Verified by `TestFlowRunConnectionParse` and `TestFlowRunConnectionMismatch`.

### Phase 2: in-process run with a stub connector

4. Resolver (`env` + `--secrets-dir`), `connhost.Manager` with an injectable `Registry` (test seam), `engine.New()`, `connector` runner (`start` is a pass-through); inject one document with `--set` properties. Verified by `TestFlowRunPrintsReturnedPayload`, `TestFlowRunSetProperties`, `TestFlowRunListenerStartIgnored`, `TestFlowRunMissingSecret` (exit 1), `TestFlowRunLiteralSecret` (exit 2, `Prepare` error classified).
5. Output and exit-code mapping, `--json` envelope and JSON errors. Verified by `TestFlowRunExitCodes`, `TestFlowRunJSONEnvelope`, `TestFlowRunStdoutStderrSeparation`, `TestFlowRunRedaction`.

### Phase 3: real SFTP and host-key policy

6. `connhost.Builtins()` wired; host-key rules of Design step 4 (`--insecure-host-key`, file-level `insecure` -> exit 2, `fingerprint`, `--known-hosts` > inline `knownHosts` > `~/.ssh/known_hosts`) applied in memory; password and key auth. Verified by `TestFlowRunSFTPInProc`, `TestFlowRunSFTPKeyAuth`, `TestFlowRunSFTPWrongPassword`, `TestFlowRunSFTPHostKeyMismatch`, `TestFlowRunInsecureHostKeyWarning`, `TestFlowRunHostKeyPrecedence`.
7. Examples `examples/flows/sftp-download.flow.json` and `examples/connections/sftp.connection.json`, plus a section in [cli.md](../cli.md) and [connectors/sftp.md](../connectors/sftp.md). Verified by `node scripts/gen-flow-reference.mjs --validate --check` and `node scripts/check-docs-links.mjs`.

### Phase 4: container e2e in CI

8. `TestFlowRunSFTPContainer` gated by `NURTUO_TEST_SFTP_E2E=1` and a CI job. Verified by that job going green and by the test skipping (not failing) when the variable is unset.

## Acceptance criteria

- WHEN `nurtuo flow run` is given a valid flow, a valid connection file for a reachable SFTP server and `--set path=/downloads/version.txt` and the file exists THEN stdout contains exactly the file's bytes, stderr contains no secret and the exit code is 0.
- WHEN the remote file does not exist THEN stdout is empty, stderr names the path (not any credential) and the exit code is 1.
- WHEN the flow fails `flow.ValidateJSON`, an argument is invalid or a `--set` value has no `=` THEN no connection is opened, diagnostics go to stderr, stdout is empty and the exit code is 2.
- WHEN a `connector` step's connection id is absent from the `--connection` files, or the kinds differ THEN the command fails with exit 2 before any network access.
- WHEN a connection file contains a literal password THEN the command fails with exit 2 and names the `secret://` fix; WHEN a referenced secret is unavailable THEN it fails with exit 1 naming the secret name, never a value.
- WHEN `--secrets-dir` holds the secret or the env var `NURTUO_SECRET_<NAME>` is set THEN the secret resolves; WHEN a private key is supplied through either THEN key auth succeeds.
- WHEN the SFTP host key is unknown or does not match the resolved known_hosts content (`--known-hosts`, else inline `knownHosts`, else `~/.ssh/known_hosts`) THEN the command fails with exit 1 and empty stdout; WHEN that content is still empty THEN exit 1 without connecting; WHEN `--insecure-host-key` is given THEN it connects and prints a warning on stderr; WHEN the connection file says `hostKeyPolicy: insecure` without the flag THEN exit 2 and no connection is made; WHEN the policy is `fingerprint` THEN `hostKeyFingerprint` is used and `--known-hosts` is not needed.
- WHEN a password, key or passphrase appears in any error THEN it is masked (`***`); WHEN the run succeeds THEN no secret appears on stderr, in logs or in diagnostics (stdout is the raw payload and is not scanned or redacted).
- WHEN `--json` is given THEN stdout is the envelope `{status, documents}` on success and empty on error, and errors on stderr are JSON.
- WHEN the flow ends in a Stop with a status THEN the exit code is 3; WHEN a document fails or the connector errors THEN the exit code is 1.
- WHEN the flow's `start` step is a `listener`/`http` type THEN it is ignored and one empty document is injected.
- WHEN the flow contains a step kind with no runner THEN the command fails with the engine's `no_runner` message and exit code 1.
- WHEN the process receives SIGINT during the transfer THEN connections are closed, partial output is not reported as success and the exit code is 1 (`TestFlowRunSIGINT`).
- WHEN `nurtuo flow run` is used THEN no listener port is opened and no control-plane request is made; WHEN `nurtuo run` is invoked THEN it behaves as before.

## Tests

Tests ship in the same change as the code. Names are proposed; none are written in this PR. Go module: `core/runtime`.

### Unit (`core/runtime/cmd/nurtuo/flowrun_test.go`)

| Test | Covers |
|---|---|
| `TestFlowRunUsage` | no args, unknown flag, unknown `flow` subcommand, malformed `--set`: exit 2, usage on stderr, empty stdout |
| `TestFlowRunInvalidFlow` | table of invalid flows (bad schema tag, missing start, unknown edge target): exit 2, no connector built |
| `TestFlowRunConnectionParse` | wrong `schema`, bad `id`, unknown field, bad `secret://` name, literal in a `writeOnly` field |
| `TestFlowRunConnectionMismatch` | kind mismatch, missing id, duplicate id across repeated `--connection` |
| `TestFlowRunMissingSecret` | env var unset: exit 1, message names the secret, not a value; `--secrets-dir` resolves it |
| `TestFlowRunLiteralSecret` | literal in a secret field: exit 2 (the `CheckLiterals` error from `Prepare` is classified, not mapped to 1) |
| `TestFlowRunSetProperties` | `--set path=...` reaches the stub connector as document property; repeated and duplicate keys |
| `TestFlowRunListenerStartIgnored` | new test fixture: `listener`/`http` start followed directly by `returnDocuments` (no Postgres; not `get-employee.flow.json`, which needs Postgres and ends in a 404 `stop`); runs with one empty document |
| `TestFlowRunPrintsReturnedPayload` | stub connector via injected `connhost.Registry`; stdout equals payload byte for byte, no trailing newline, 1 MiB payload |
| `TestFlowRunJSONEnvelope` | `--json` success envelope, JSON error on stderr, multiple documents |
| `TestFlowRunMultipleDocumentsRaw` | more than one returned document without `--json`: exit 1 |
| `TestFlowRunExitCodes` | success 0, `Result.Failed` 1, connector error 1, `no_runner` 1, ctx cancel 1, `Result.Stop` status 404 -> 3 |
| `TestFlowRunRedaction` | stub connector echoes the password in its error: stderr shows `***` |
| `TestFlowRunStdoutStderrSeparation` | logs only on stderr; stdout is the payload |
| `TestFlowRunInsecureHostKeyWarning` | `--insecure-host-key` sets the policy in memory and warns on stderr; files unchanged |
| `TestFlowRunHostKeyPrecedence` | file `insecure` without flag: exit 2, no connect; `fingerprint` ignores `--known-hosts`; `--known-hosts` beats inline `knownHosts`; inline kept when no flag; empty content: exit 1 |
| `TestFlowRunSIGINT` | SIGINT mid-run (blocking stub connector): connections closed, exit 1, stdout empty |

### Integration with an in-process SSH server (`core/runtime/cmd/nurtuo/flowrun_sftp_test.go`)

| Test | Covers |
|---|---|
| `TestFlowRunSFTPInProc` | server pattern from `connectors/sftp/sftp_test.go` (`startServer`, copied: it is a `_test.go` helper of another module); password auth, GET returns the file |
| `TestFlowRunSFTPKeyAuth` | private key from `--secrets-dir` |
| `TestFlowRunSFTPWrongPassword` | exit 1, password absent from output |
| `TestFlowRunSFTPHostKeyMismatch` | `--known-hosts` with another key: exit 1, empty stdout |

### E2E with a container (gated)

`TestFlowRunSFTPContainer` in `core/runtime/test/e2e/flowrun_sftp_test.go` (**[ASSUMPTION]** location).

- **Gate**: skipped unless `NURTUO_TEST_SFTP_E2E=1` and `docker info` succeeds. Pattern: the Postgres tests in `connectors/postgres/postgres_test.go`, whose `liveSettings` helper reads `NURTUO_TEST_POSTGRES_URL` and calls `t.Skip` when it is unset.
- **Image** **[ASSUMPTION]**: `atmoz/sftp` pinned by digest (or `linuxserver/openssh-server`). No `testcontainers` dependency exists, so the test runs `docker run -d -p 127.0.0.1::22` through `os/exec` and reads the port with `docker port`.
- **Fixtures**: `t.TempDir()` holds a generated ed25519 host key (mounted), the flow copied from `examples/flows/sftp-download.flow.json`, a connection file written from a template with the mapped port, a generated `known_hosts`, and `downloads/version.txt` with a random body bind-mounted into the user's home. The test never touches a real server.
- **Cleanup**: `t.Cleanup` runs `docker rm -f <id>`.
- **Cases**: (1) password auth + `--known-hosts`, `--set path=/downloads/version.txt` returns the body, exit 0; (2) key auth; (3) missing file; (4) wrong host key; (5) wrong password; stdout, stderr and exit codes asserted in each.
- **Run**: `cd core/runtime && NURTUO_TEST_SFTP_E2E=1 GOWORK=off go test -race -run TestFlowRunSFTPContainer ./test/e2e/...`
- **CI**: a job in `.github/workflows/ci.yml` on `ubuntu-latest` (Docker present) sets `NURTUO_TEST_SFTP_E2E=1`.

### Other

- Docs: `node scripts/check-docs-links.mjs` and `node scripts/gen-flow-reference.mjs --validate --check` (the new example flow must validate).
- Boundary: `./scripts/check-boundaries.sh` must still pass.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The user types `nurtuo run flow.json`; today `nurtuo run` ignores positionals and exits 2 with usage (`fs.NArg() > 0`). | low | UX, naming | The usage error of `nurtuo run` should hint `nurtuo flow run` (implementation PR); otherwise unchanged |
| 2 | The user puts the SFTP password literally in the connection file; `(*connections.Definition).CheckLiterals` rejects it and the user switches to `--insecure-host-key` or scripts with the password in argv. | medium | usability, security | Error message gives the exact `secret://` fix and env var name; example in cli.md |
| 3 | `settings.knownHosts` expects content; a user passes a path. The connector fails with `invalid knownHosts`, and the SFTP spec's gap 4 (key algorithm / `[host]:port` form) bites on non-22 ports. | medium | usability | `--known-hosts` reads the file; diagnostics name the file and host:port |
| 4 | A canvas-exported flow lacks the `setProperties` `path` step because `ConnectorConfig` has no path field; SFTP `GET` fails with `errBadInput`. | medium | contract | Decision 12 supplies `path` through `--set`; example flow and a hint in the error message |
| 5 | A hostile flow file is run with real SFTP credentials and `DELETE`/`RENAME` operations; remote files are deleted. | high | security | Document in cli.md; `--read-only` is a follow-up |
| 6 | stdout is a 4 GiB file; `docs.Spool` fills `/tmp`, or stdout is a closed pipe and the command hangs. | medium | reliability | `io.Copy`, `EPIPE` handling, temp dir cleanup; test with a 64 MiB payload |
| 7 | The SFTP `timeoutMs` covers only connect + handshake (SFTP spec gap 1); a stalled server hangs the command. | medium | reliability | `--timeout` bounds the whole run via context |
| 8 | Bad auth fails the whole `Execute` stream (SFTP spec gap 6); it could be mapped to the wrong exit code. | low | exit codes | All execution errors are exit 1 (Decision 8); tests classify the message, not the code |
| 9 | Binary output on a TTY corrupts the terminal. | low | UX | **[ASSUMPTION]** refuse non-UTF-8 to a TTY without `--json`; non-blocking |
| 10 | Drafting assumptions (base64 payload in the envelope, multiple documents in raw mode fail, Stop prints nothing on stdout, `--timeout`, env-then-dir secret order) differ from what the user expects. | low | contract | Experimental contracts; change in the implementation PR with a changelog line; non-blocking |
| 11 | Running multiple connectors (`postgres`, `nats`) from a one-shot process leaves side effects (writes, published messages) with no dry run. | medium | safety | Document; `--dry-run` as a follow-up |
| 12 | A canvas flow uses CEL `&&` / `||` in a `decision`/`route` condition; `BasicEvaluator` does not handle them, so the step errors at run time (exit 1). | low | engine | Out of scope; documented limitation, fix belongs to the evaluator |
| 13 | A connection says `hostKeyPolicy: insecure` and the user expects it to work. | low | UX, security | Exit 2 with a message naming `--insecure-host-key` (Decision 6); no second opt-out |

## Improvement opportunities

- `--read-only` guard rejecting mutating operations; `--dry-run` that runs connection `Test` only.
- `$NURTUO_CONNECTION_DIR` so one directory per environment.
- `--output <file>` for binary payloads.
- A thin `nurtuoctl run` wrapper that `exec`s `nurtuo flow run` (separate spec if wanted).
- Reuse for canvas "test mode" (Epic 10 not met per the canvas-editor spec).

## References

- [ARCHITECTURE.md](../ARCHITECTURE.md), [getting-started.md](../getting-started.md), [cli.md](../cli.md), [connectors/sftp.md](../connectors/sftp.md), [flow-reference.md](../flow-reference.md)
- [ADR 0010: connections and secrets](../adr/0010-connections-and-secrets.md)
- Specs: [connector-sftp](implemented/2026-11-21-connector-sftp.md), [engine-interpreter](implemented/2026-11-21-engine-interpreter.md), [ts-client-and-cli](implemented/2026-11-21-ts-client-and-cli.md), [canvas-editor](implemented/2026-11-21-canvas-editor.md), [license-boundaries](implemented/2026-11-21-license-boundaries.md)
- Code: `core/runtime/cmd/nurtuo/{main,run}.go`, `core/runtime/internal/connections/connections.go`, `core/runtime/internal/connhost/{registry,manager}.go`, `core/runtime/internal/engine/{interp,steps}.go`, `connectors/sftp/{sftp,config}.go`, `sdk/go/flow/flow.go`, `examples/flows/reference/connector.flow.json`

## Changelog

- 2026-11-23: created as draft skeleton
- 2026-11-23: approved; decisions recorded; command is nurtuo flow run
- 2026-11-23: address review: host-key precedence replaces file-level `insecure` assumption (exit 2 without flag), exit-code classification for secrets, Stop never prints the envelope, listener-start test fixture, test names, corrected citations
