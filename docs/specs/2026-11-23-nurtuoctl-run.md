<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: `nurtuoctl run` (run a canvas-exported flow locally against a connection file)

Status: draft. Date: 2026-11-23.

Extends: [connector-sftp](implemented/2026-11-21-connector-sftp.md),
[engine-interpreter](implemented/2026-11-21-engine-interpreter.md),
[ts-client-and-cli](implemented/2026-11-21-ts-client-and-cli.md).

> **Draft skeleton.** The Open Questions block gates Design. Everything marked **[ASSUMPTION]** is a
> reversible default used only to make the draft concrete; it needs Łukasz's confirmation. Nothing
> here may be implemented until Status is `approved`.

## TLDR

Run a flow JSON exported from the canvas, plus a connection file of kind `nurtuo.connection/v1`,
on the engine **in-process** with the **built-in SFTP connector**, and print the resulting document
to stdout. No control plane, no deploy. Motivating case: `GET /downloads/version.txt` from Łukasz's
own SFTP server. The one thing to know: **no `run` for flow files exists today and the name is
contested**: `nurtuoctl` is a TypeScript CLI that cannot link the Go engine, and `nurtuo run` already
means "run the runtime in the foreground".

## Goal

A developer with a flow file and a connection file runs one command and sees the output document of
the flow on stdout, executed by the real engine and the real SFTP connector, without a control plane,
bundle or deployment.

## Open Questions

Answer these before the spec can move to `approved`. Q1 and Q2 decide where the code lives.

- **Q1 Binary/command name.** `nurtuoctl run` (TS CLI, `cli/`), `nurtuo run <flow>` (Go binary), or a new `nurtuo flow run`? Today `nurtuo run` is taken (see Design) and `nurtuoctl` is TypeScript.
- **Q2 Where does the code live?** (a) Go, in `core/runtime/cmd/nurtuo` (AGPL, can use the engine in-process); (b) TS `nurtuoctl run` that `exec`s a `nurtuo` binary found on PATH; (c) a new Go binary. (TS cannot link the engine; `scripts/check-boundaries.sh` forbids Apache `cli/` depending on `core/`.)
- **Q3 Connection-file location and format.** One file passed with `--connection <file>` holding a single `nurtuo.connection/v1` document? Or a directory/lookup by the step's `connection` id? Single flag, repeatable flag, or both?
- **Q4 Connection kind.** Reuse `nurtuo.connection/v1` unchanged (`id`, `connector`, `settings`, `secret://` references)? Or may local files hold literal passwords (needs a different/relaxed kind or a flag, because `connections.CheckLiterals` rejects literals in secret-marked fields)?
- **Q5 Credentials: password vs key.** Which are supported in M1: password only, key only, or both? How does the user supply them: `secret://` resolved from `NURTUO_SECRET_*` env, a `dir` secrets folder, `--secrets-dir`, a prompt, or a key file path?
- **Q6 Host-key verification default.** Keep the connector default `known_hosts` (requires the `knownHosts` content in settings)? Add `--known-hosts <file>` that fills it from `~/.ssh/known_hosts`? Allow `hostKeyPolicy: insecure` locally, and if so only with an explicit flag (e.g. `--insecure-host-key`) or a warning on stderr?
- **Q7 stdout content.** Raw document bytes only, a JSON envelope (`{status, documents:[{id, properties, payload}]}`), or a path to a temp file? What about binary payloads and multiple returned documents?
- **Q8 Exit codes.** Adopt the `nurtuoctl` table (0 ok, 1 failure, 2 usage, 3 network, 4 auth) or the Go binary's (0, 1, 2)? Separate codes for flow-validation error, connector/application error and Stop with status?
- **Q9 Error output.** On failure: nothing on stdout and a message on stderr, or an error document on stdout (same envelope as success)? Should `--json` switch the stderr format as in `docs/cli.md`?
- **Q10 Scope of M1.** SFTP-only (the `sftp` connector hard-wired), or any connector in `connhost.Builtins()` (`http`, `sftp`, `postgres`, `nats`) from day one? Bridge connectors out of scope?
- **Q11 Is today's canvas export a valid input?** A flow that downloads a file needs a `setProperties` step with `path` before the `connector` step (see Design); can the canvas produce that, and is a `start` step of type `listener`/`http` (like `examples/flows/get-employee.flow.json`) accepted by `run` (ignored, one empty document injected)? Or must the flow use `start.type: manual`?
- **Q12 Path parameter.** How does the user say `/downloads/version.txt`: inside the flow (`setProperties` `path`), via `--set path=/downloads/version.txt` (initial document property), or via the connection `rootPath`?
- **Q13 Splitting.** Is "run a flow locally" one capability, or two: (1) a generic local runner for any flow/connector, (2) the SFTP-specific wiring? Split into separate specs?
- **Q14 Test infrastructure.** Is a container-based e2e (Docker required) acceptable in CI, gated by an env var like `NURTUO_TEST_POSTGRES_URL`, or must it run in plain `go test`?

## Scope

In scope (subject to the questions above):

- A command that loads a flow file and a connection file, validates both, builds a one-connector
  registry, runs the engine in-process and prints the returned document(s) to stdout.
- The SFTP connector (`connectors/sftp`) as the connector exercised end to end.
- Local secret handling and host-key verification for SFTP.
- Exit codes and stdout/stderr contract; unit tests and one container e2e.

Non-goals:

- Deploying, pushing or talking to the control plane; no bundle, signing or gateway.
- Listeners and triggers: the `start` step's `type`/`path` is not served; no HTTP listener, queue or cron.
- Running more than once, watching files, interactive prompts for secrets (unless Q5 says so).
- Uploading/deleting/renaming remote files as a feature (the connector supports them; the spec only needs `GET`, and mutating operations remain whatever the flow says).
- Bridge connectors, `queue`/`notify` step kinds (no runner exists on `main` per getting-started).
- Changing `nurtuo.flow/v1`, `nurtuo.connection/v1` or the SFTP connector.

## Status and links

| | |
|---|---|
| Pull requests | this draft only; implementation PRs after approval |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) |
| Issues | none |
| Code (existing) | `core/runtime/cmd/nurtuo/`, `core/runtime/internal/{engine,connections,connhost,secrets}/`, `connectors/sftp/`, `sdk/go/flow/`, `cli/src/` |
| User docs | [cli.md](../cli.md), [connectors/sftp.md](../connectors/sftp.md), [getting-started.md](../getting-started.md) |

## Design (proposed, grounded in what exists today)

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
  `connections.CheckLiterals` rejects literal values in secret-marked fields (`writeOnly`).
  `connhost.Manager.Prepare` validates, resolves, builds and `Test`s connections for a deployment and
  `Manager.Lookup` serves them to the `connector` step.
- **Connector registry**: `connhost.Builtins()` (`core/runtime/internal/connhost/registry.go`)
  registers `http`, `sftp`, `postgres`, `nats`; `cmd/nurtuo/run.go` exposes it as `nativeConnectors()`.
- **SFTP connector**: `connectors/sftp` (`sftp.go`, `config.go`, `schemas/connection.json`; Apache-2.0).
  `GET` streams a file as output document `<id>.out` (properties `path`, `filename`; tracked `size`).
  The path comes from the input document property `path` (or step option `directory` for uploads).
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
| `connectors/sftp/schemas/connection.json` | password or key, "both allowed" in `sshConfig` | `oneOf` rejects both set (SFTP spec gap 9). **On this path** if Q5 allows both. Unclear whether intentional: see Q5. |

No existing spec describes a local "run a flow file" command, so there is no spec-vs-code drift for the
command itself.

### Proposed flow of the command **[ASSUMPTION, shape depends on Q1/Q2]**

Assuming Q2(a): a new self-registered subcommand in `core/runtime/cmd/nurtuo` (new file, e.g. `runflow.go`).

1. Parse args: `<flow.json> --connection <file>` (Q3), optional `--set key=value` (Q12), `--timeout`.
2. Read flow; `flow.ValidateJSON` then `flow.Parse`. Validation errors: exit per Q8, nothing on stdout.
3. Read connection file; `connections.Parse`. Reject a flow whose `connector` steps reference a
   connection id or connector that the file does not satisfy (same rule as `connhost.Manager.Prepare`).
4. Resolve secrets via `connections.Resolver` with the `env` provider **[ASSUMPTION, Q5]**; `Redactor` active.
5. Build `connhost.NewManager(connhost.Builtins(), resolver)`; `Prepare` + `Commit` for a synthetic deployment, which `Test`s the connection (fail fast on auth or host key error); `defer Close`.
6. `engine.New()`; register `start` (no-op), `connector` (`RegisterConnectorLookup(mgr)`), `map`, `setProperties`, `message`. Steps of other kinds fail with the engine's `no_runner`.
7. Inject **one empty document** (plus `--set` properties) into the `start` step and `Run` under a signal-aware context.
8. Print `Result.Returned` per Q7; print errors per Q9; return exit code per Q8. Close all sets.

### Output format **[ASSUMPTION, Q7: raw payload]**

For the motivating case the stdout is exactly the bytes of `/downloads/version.txt`, nothing else.
If more than one document is returned, the default is to fail with a usage error until Q7 is answered.
Logs and diagnostics go to stderr only (`slog` text handler as in `run.go`).

## Contracts (all **experimental**)

| Contract | Proposal | Depends on |
|---|---|---|
| Command | `<binary> run <flow.json> --connection <file> [--set k=v] [--timeout d]` | Q1, Q2, Q3, Q12 |
| Flow input | `nurtuo.flow/v1` as exported by the canvas (`<id>.flow.json`); `.layout.json` not needed | Q11 |
| Connection input | `nurtuo.connection/v1` (`id`, `connector`, `settings`), parsed by `connections.Parse` | Q3, Q4 |
| Secrets | `secret://name` resolved from env `NURTUO_SECRET_<NAME>` (existing `env` provider) | Q5 |
| stdout | raw payload of the single returned document | Q7 |
| stderr | `nurtuo: <message>` text, diagnostics, never secrets | Q9 |
| Exit codes | 0 success; 1 failure; 2 usage; more codes open | Q8 |
| Environment | `NURTUO_SECRET_*` only | Q5 |

## Compatibility

No existing contract changes: `nurtuo.flow/v1`, `nurtuo.connection/v1`, the SFTP connector and
the runtime config are used as they are. Name conflicts: if Q1 picks `nurtuo run`, the existing
`nurtuo run [--config path]` (runtime in the foreground, documented in
[operations.md](../operations.md) and getting-started) would break unless the new form is
distinguished by an argument (a flow file vs. no positional) or placed under another name. Picking
`nurtuoctl run` has no conflict inside `cli/` (no such command), but see Q2.

## Security

- **Secrets never in files.** Connection files hold `secret://` references; the resolver injects the value in memory (ADR 0010). `Secret` renders `***`; `Redactor` scrubs errors and logs. Output document content is **not** redacted (it is user data; a downloaded file may contain secrets), so stdout must never be sent to logs by the command.
- **Password vs key** (Q5). Passwords in env are visible to same-user processes (`/proc/<pid>/environ`); a `dir` provider requires 0700/0600. A private key is a multi-line PEM; env is awkward, a `dir` secret file fits better. No secret is accepted as an argv value (same rule as `nurtuo secret`).
- **Host-key verification** (Q6). Default must stay `known_hosts`; `insecure` must not become an implicit local default. The connector wants the known_hosts **content** in `settings.knownHosts` (not a path), so a local run needs either a settings value or a flag that reads a file into it.
- **Path confinement.** `rootPath` confinement of `connectors/sftp` applies; a flow can still read any file under `rootPath`.
- **Untrusted flow.** A flow file can contain `script` steps and map expressions; with the `connector` wiring only, no network beyond the connection's host is used. `script` steps fail (`no_runner`) unless Q10 enables them.
- **Output to terminal.** Printing a binary or hostile file to a terminal can inject escape sequences; consider refusing non-UTF-8 to a TTY (open: Q7).
- **Resource limits.** A large remote file streams through `docs.Spool` (spills to disk above a threshold); a temp dir and cleanup are needed.

## Step plan (for a later implementation PR, **not in this PR**)

Phases are independently shippable. Each step leaves the system working and names its verification.

### Phase 1: parse and validate (no network)

1. Add the subcommand skeleton, usage text, flag parsing; unknown flag/missing arg exit 2. Verified by `TestRunFlowUsage`.
2. Load and validate flow with `flow.ValidateJSON`/`flow.Parse`; print diagnostics to stderr, exit 1. Verified by `TestRunFlowInvalidFlow`.
3. Load connection with `connections.Parse`; cross-check the flow's `connector` steps. Verified by `TestRunFlowConnectionMismatch`.

### Phase 2: in-process run with a fake connector

4. Wire resolver (`env`), `connhost.Manager` with an injected `Registry` (test seam), `engine.New()`, `start`/`connector` runners; inject one document. Verified by `TestRunFlowPrintsReturnedPayload` using a stub `connector.Connector`.
5. Output and exit code mapping for success, `Result.Failed`, `Result.Stop`, context cancel. Verified by `TestRunFlowExitCodes`.

### Phase 3: real SFTP

6. Use `connhost.Builtins()`; host-key and credential handling per Q5/Q6. Verified by `TestRunFlowSFTPInProc` (in-process SSH server from `connectors/sftp/helper_test.go` pattern) and by the e2e below.
7. Add `examples/flows/sftp-download.flow.json` and `examples/connections/sftp.connection.json` (new example files, validated by `node scripts/gen-flow-reference.mjs --validate --check`) and a user doc section. Verified by `make docs`.
8. Container e2e `TestRunFlowSFTPContainer` (gated). Verified by CI job per Q14.

### Phase 4 (only if Q2(b)): `nurtuoctl run` wrapper

9. TS command that locates and `exec`s `nurtuo`, forwarding args, stdio and exit code. Verified by `cli/src/unit.test.ts` with a fake binary on PATH.

## Acceptance criteria

- WHEN `run` is given a valid flow and a valid connection file for a reachable SFTP server and the file `/downloads/version.txt` exists THEN stdout contains exactly the file's bytes and the exit code is 0.
- WHEN the remote file does not exist THEN stdout is empty, stderr names the path (not any credential) and the exit code is non-zero (value per Q8).
- WHEN the flow file fails `flow.ValidateJSON` THEN no connection is opened, diagnostics go to stderr and the exit code is non-zero.
- WHEN the connection's `connector` differs from the step's `connector`, or the step names a connection id that is absent THEN the command fails before any network access.
- WHEN a secret referenced as `secret://<name>` is not available THEN the command fails naming the secret name, never a value.
- WHEN the SFTP host key is unknown or does not match under `known_hosts` THEN the command fails and prints nothing to stdout; WHEN `hostKeyPolicy` is `insecure` THEN a warning is printed to stderr (Q6).
- WHEN the password, key or passphrase appears in any error THEN it is masked (`***`); WHEN the run succeeds THEN neither value appears on stdout or stderr.
- WHEN the flow contains a step kind with no runner THEN the command fails with the engine's `no_runner` message and a non-zero exit code.
- WHEN the process receives SIGINT during the transfer THEN the connection is closed, partial output is not reported as success and the exit code is non-zero.
- WHEN `run` is used THEN no listener port is opened and no control-plane request is made.

## Tests

Tests ship in the same change as the code. Proposed names; not written in this PR. Go module: `core/runtime` unless noted.

### Unit (`core/runtime/cmd/nurtuo/runflow_test.go`)

| Test | Covers |
|---|---|
| `TestRunFlowUsage` | no args, unknown flag, missing `--connection`: exit 2, usage on stderr, nothing on stdout |
| `TestRunFlowInvalidFlow` | table of invalid flows (bad schema tag, missing start, unknown edge target): exit 1, diagnostics, no connector built |
| `TestRunFlowConnectionParse` | table: wrong `schema`, bad `id`, unknown field, bad `secret://` name, literal in a `writeOnly` field |
| `TestRunFlowConnectionMismatch` | connector id mismatch, missing connection id, two candidates and no default |
| `TestRunFlowMissingSecret` | env var unset: message names the secret, not a value |
| `TestRunFlowPrintsReturnedPayload` | stub connector via injected `connhost.Registry`; stdout equals payload byte for byte, including a payload with no trailing newline and a 1 MiB payload |
| `TestRunFlowMultipleReturnedDocuments` | behaviour fixed by Q7 |
| `TestRunFlowExitCodes` | success, `Result.Failed`, `Result.Stop` (status 404), connector error, `no_runner`, ctx cancel |
| `TestRunFlowRedaction` | stub connector echoes the password in its error: stderr shows `***` |
| `TestRunFlowStdoutStderrSeparation` | logs only on stderr; stdout is the payload |

### Integration with the in-process SSH server (`core/runtime/cmd/nurtuo/runflow_sftp_test.go`)

| Test | Covers |
|---|---|
| `TestRunFlowSFTPInProc` | uses the in-memory SFTP server pattern from `connectors/sftp/helper_test.go` (`startServer`) [reuse by copy: it is a `_test.go` helper of another module]; fingerprint policy, password auth, GET returns the file |
| `TestRunFlowSFTPWrongPassword` | exit non-zero, password absent from output |
| `TestRunFlowSFTPHostKeyMismatch` | `known_hosts` with another key: fails, empty stdout |

### E2E with a container (gated)

`TestRunFlowSFTPContainer` in `core/runtime/test/e2e/runflow_sftp_test.go` (**[ASSUMPTION]** location).

- **Image** **[ASSUMPTION, confirm Q14]**: `atmoz/sftp` pinned by digest, or `linuxserver/openssh-server`; no `testcontainers` dependency exists in the repo, so start it with `docker run -d -p 127.0.0.1::22` through `os/exec` and read the mapped port with `docker port`.
- **Fixtures, self-contained**: `t.TempDir()` holds a generated ed25519 host key (mounted into the container), the flow file copied from `examples/flows/sftp-download.flow.json`, a connection file written from a template with the mapped port, a generated `known_hosts`, and a `downloads/version.txt` with a random body bind-mounted into the user's home.
- **Cleanup**: `t.Cleanup` runs `docker rm -f <id>`; the test never touches the user's real server.
- **Cases**: (1) password auth + `known_hosts` GET returns the body, exit 0; (2) key auth (if Q5 allows); (3) missing file; (4) wrong host key; (5) wrong password; stdout/stderr and exit codes asserted in each.
- **Gate**: skipped unless `NURTUO_TEST_SFTP_E2E=1` and `docker info` succeeds (same pattern as `NURTUO_TEST_POSTGRES_URL` in `connectors/postgres/postgres_test.go`).
- **Run**: `cd core/runtime && NURTUO_TEST_SFTP_E2E=1 GOWORK=off go test -race -run TestRunFlowSFTPContainer ./test/e2e/...`
- CI: a job in `.github/workflows/ci.yml` on `ubuntu-latest` (Docker present), or nightly only (Q14).

### Other

- If Q2(b): `cli/src/unit.test.ts` cases `run forwards args and exit code`, `run reports missing nurtuo binary with exit 2`; `node --test "dist/**/*.test.js"` via `pnpm --filter @nurtuo/cli test`.
- Docs: `node scripts/check-docs-links.mjs` and `node scripts/gen-flow-reference.mjs --validate --check` (new example flow must validate).
- Boundary: `./scripts/check-boundaries.sh` must still pass.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The user types `nurtuo run flow.json` expecting a one-off run; today `nurtuo run` ignores positionals and exits 2 with usage (`fs.NArg() > 0`). Conversely, choosing the same verb risks an operator accidentally starting a second runtime. | medium | UX, naming | Q1 |
| 2 | A TS `nurtuoctl run` cannot link the Go engine (`internal` packages, AGPL vs Apache boundary); it would need an installed `nurtuo` of a matching version. A version skew silently changes flow semantics. | high | architecture | Q2; version check against `nurtuo version` |
| 3 | The user puts the SFTP password literally in the connection file; `connections.CheckLiterals` rejects it, and the user concludes the tool is broken and switches to `hostKeyPolicy: insecure` or to scripts with the password in argv. | medium | usability, security | Q4/Q5; clear error message with the exact `secret://` fix and env var name |
| 4 | `settings.knownHosts` expects content; a user passes a path (`~/.ssh/known_hosts`). The connector fails with `invalid knownHosts` and the SFTP spec's gap 4 (key algorithm / `[host]:port` form) bites on non-22 ports. | medium | usability | Q6; `--known-hosts` flag, better diagnostics |
| 5 | The canvas-exported flow lacks the `setProperties` `path` step because `ConnectorConfig` has no path field; the SFTP `GET` fails with `errBadInput` (missing `path`) and the flow "does nothing useful". | high | contract | Q11, Q12; example flow, validation hint |
| 6 | A hostile flow file (downloaded from elsewhere) is run with the user's real SFTP credentials and `DELETE`/`RENAME` operations; remote files are deleted. | high | security | Document; consider `--read-only` (rejects mutating operations) as an open follow-up |
| 7 | stdout is a 4 GiB file; `docs.Spool` spills to the temp dir and fills `/tmp`, or the output is piped to a closed reader and the command hangs. | medium | reliability | Stream with `io.Copy`, handle `EPIPE`, temp dir cleanup; test with a 64 MiB payload |
| 8 | The SFTP connector's `timeoutMs` covers only connect + handshake (SFTP spec gap 1); a stalled server hangs the command forever. | medium | reliability | `--timeout` bounds the whole run via context |
| 9 | Bad auth fails the whole `Execute` stream (SFTP spec gap 6); the CLI maps it to the wrong exit code (network vs auth). | low | exit codes | Q8; classify by error type in tests |
| 10 | Output on a TTY of a binary file corrupts the terminal. | low | UX | Q7 |

## Improvement opportunities

- `--read-only` guard; `--dry-run` that runs `Test` only (connect and stat) and prints nothing else.
- `nurtuoctl run --connection` read from `$NURTUO_CONNECTION_DIR` so one file per environment.
- Other connectors (`http`, `postgres`) at no extra design cost once Q10 allows the registry.
- `--output <file>` for binary payloads; `--format json` envelope.
- Reuse for `nurtuoctl test` (canvas "test mode", Epic 10 not met per the canvas-editor spec).

## References

- [ARCHITECTURE.md](../ARCHITECTURE.md), [getting-started.md](../getting-started.md), [cli.md](../cli.md), [connectors/sftp.md](../connectors/sftp.md), [flow-reference.md](../flow-reference.md)
- [ADR 0010: connections and secrets](../adr/0010-connections-and-secrets.md)
- Specs: [connector-sftp](implemented/2026-11-21-connector-sftp.md), [engine-interpreter](implemented/2026-11-21-engine-interpreter.md), [ts-client-and-cli](implemented/2026-11-21-ts-client-and-cli.md), [canvas-editor](implemented/2026-11-21-canvas-editor.md), [license-boundaries](implemented/2026-11-21-license-boundaries.md)
- Code: `core/runtime/cmd/nurtuo/{main,run}.go`, `core/runtime/internal/connections/connections.go`, `core/runtime/internal/connhost/{registry,manager}.go`, `core/runtime/internal/engine/{interp,steps}.go`, `connectors/sftp/{sftp,config}.go`, `sdk/go/flow/flow.go`, `examples/flows/reference/connector.flow.json`

## Changelog

- 2026-11-23: created as draft skeleton
