<!-- SPDX-License-Identifier: Apache-2.0 -->
# Pre-implement: canvas Run (test-run a flow from the canvas against a real connection)

**Readiness:** no-go
**Spec:** docs/specs/2026-11-23-canvas-run.md

Date: 2026-11-23. Analysis only: the spec, the code and tracker state were not changed. Code was
read on `spec/canvas-run` (based on `ad72ae2`), on `origin/main` (`d5adeeb`), and on the PR
branches `origin/feat/nurtuo-flow-run` (PR #51), `origin/ci/playwright-e2e` (PR #53) and
`origin/docs/agents-md` (PR #49, `AGENTS.md`).

## Blockers

1. **Status is `draft` with 10 blocking decisions open** (D1, D3, D4, D6, D7, D8, D9, D10, D11,
   D16). The spec cannot be implemented until they are answered (AGENTS.md "Never start code for
   sizeable work whose spec is `draft`"; spec-writing skill: Open Questions gate).
2. **Scope split not decided** (D14 pending). Phases 1, 2 and 5 are independently shippable
   capabilities (CLI `steps` output, workspace connections/secrets API, `nurtuo flow host-key`). The
   readiness gate requires the split decision before approval, even though the spec marks D14
   non-blocking.
3. **The spec misdescribes the code in places that drive the design and the acceptance criteria.**
   The full list is in [Factual errors in the spec](#factual-errors-in-the-spec). The ones that
   change behaviour: the SFTP list operation is `QUERY`, not `LIST`; no-auth mode answers 405, not
   404; `EnvVar()` already adds the `NURTUO_SECRET_` prefix; the CLI emits no `timeout` code and no
   structured stop status; host-key and password failures happen before the engine runs, so they
   come with no per-step trace.

## Rules and documents loaded

- `AGENTS.md` (from `origin/docs/agents-md`, PR #49, not on this branch): Always / Ask First / Never,
  Task Router rows for `core/`, `core/web`, `connectors/`, CLI, docs.
- [docs/specs/README.md](../README.md), [_template.md](../_template.md), the spec-writing review
  checklist, the [backward-compatibility contract](../2026-11-22-backward-compatibility-contract.md)
  (draft).
- Lessons (`origin/docs/agents-md:docs/lessons/README.md`): the catalog lists only
  `proto-message-name-descriptor-breaks-csharp` (proto/C#), which does not match this spec. The
  uncatalogued `ci-label-gates-and-duplex-grpc-tests.md` covers label-gated CI triggers and gRPC
  duplex tests. It does not match Phase 6 either, because the spec adds no label gate. Neither
  record was applied.
- Issues #54, #55, #56 (all open) and the state of PRs #49, #51 and #53 (open) and #50 (merged
  2026-11-23T17:14Z as `d5adeeb`).

## Codebase grounding (spec claim -> code)

| Spec claim | Verified in | Result |
|---|---|---|
| Canvas toolbar, palette labels, `fieldsOf`, free-text `connection`, `FlowStore` + `validateFlow`, no `fetch`, `@nurtuo/client` not imported | `core/web/src/ui/App.tsx`, `ConfigDialog.tsx`, `model/{catalog,formSchema,flowStore}.ts`, grep of `core/web/src` | matches |
| Palette ids `start-1`, `connector-1`, `returndocuments-1` | `model/catalog.ts:48-51` (`kind.toLowerCase()-i`) | matches |
| Auth hook order, `endpoints` keyed by policy action, `EndpointCtx.workspace`, legacy routes without workspace | `auth/middleware.ts` `inner()`/`run()`, `auth/endpoints.ts` | matches; `extraEndpoints` is a feasible seam |
| `policy.test.ts` fails on an undocumented operation and requires 401/403 | `auth/policy.test.ts` | matches |
| Permissions mirrored by `0103`, drift test in `auth/crypto.test.ts` | `auth/permissions.ts`, `migrations/0103_auth_seed_roles.sql`, `crypto.test.ts:84-92` | matches, but see finding I1 (regex) |
| No body limit in `createHttpServer` | `api.ts` `createHttpServer` | matches |
| With `auth: "disabled"` new routes are 404 | `api.ts` last line of `route()` | **wrong**: 405 (F2) |
| `Engine.Run` signature, `Result{Returned, Failed, Stop, Trace}`, `StepTrace` fields, trace returned on error | `core/runtime/internal/engine/interp.go:57-102` | matches |
| `flowRun` pipeline, envelope, exit codes 0/1/2/3, SIGTERM via `signal.NotifyContext` | PR #51 `core/runtime/cmd/nurtuo/flowrun.go` | matches, with nits F12 |
| JSON error codes that the canvas maps (`timeout`, `stop.status`) | PR #51 `flowrun.go` `fail()`/`reportError()` | **wrong** (F6) |
| `secret://` name pattern, `EnvVar` upper-cases and maps other chars to `_` | `internal/secrets/secrets.go:25`, `env.go:26-37` | matches; `EnvVar` includes the prefix (F3) |
| SFTP settings, policies, fingerprint callback message | `connectors/sftp/config.go`, `schemas/connection.json` | matches (omits `maxListEntries`) |
| SFTP operations `GET` and `LIST` | `connectors/sftp/connector.yaml`, `sftp.go:55-68`, `engine/steps.go:278-292` | **wrong**: `QUERY` (F1) |
| `GET` emits `<step>.out` | `sftp.go:584` (`d.id + ".out"`), `engine/steps.go:206-236` | **wrong** (F4) |
| `controlPlaneEnv(dsn, creds, port, gatewayAddr)`, `--control-plane-cmd node` | `cmd/nurtuo/standalone.go:140,207-221` | matches; supervisor appends to `os.Environ()` (`standalone_supervisor.go:96`) |
| PR #53 `web-e2e`: Node 24, chromium, builds `nurtuo`, pinned `atmoz/sftp`, random masked password, `ssh-keyscan`, `NURTUO_SECRET_SFTP_PASSWORD` | `origin/ci/playwright-e2e:.github/workflows/ci.yml:90-162` | matches |
| `canvas.spec.ts` has 10 tests | `core/web/e2e/canvas.spec.ts` | matches |
| PRs #50/#51/#53 not merged | `gh pr view` | **stale**: #50 merged (F11) |

## Findings

### Critical

- **Critical: readiness gate fails (draft, blocking questions, undecided split).** Evidence: spec
  header `Status: draft`, Decisions table "Blocking: yes" on D1, D3, D4, D6–D11, D16; D14 pending →
  Fix: Łukasz answers the rows listed in [What would unblock approval](#what-would-unblock-approval),
  then the author folds the answers in, removes the `[DEFAULT, pending Łukasz]` markers and sets
  `approved`.
- **Critical: the D11 allowlist names a non-existent operation.** Evidence: SFTP declares
  `QUERY, GET, CREATE, UPSERT, DELETE, EXECUTE/RENAME` (`connector.yaml`, `sftp.go:55-68`).
  `engine.operationType` maps anything other than GET/QUERY/CREATE/UPDATE/UPSERT/DELETE to
  `EXECUTE` with that custom type (`steps.go:278-292`), so `operation: LIST` fails at run time with
  `sftp: unsupported operation`, while a real directory listing (`QUERY`) is refused as
  `not_allowed_in_test_run` → Fix: allow `GET` and `QUERY`. Compare after `toUpperCase()`, as the
  engine does (`strings.ToUpper(op)`), and test `get`/`Get`/`query` as well as `RENAME`, `DELETE`,
  `CREATE`, `UPSERT`, `UPDATE`.
- **Critical: no-auth behaviour is misdescribed.** Evidence: `route()` ends with
  `throw new ApiError(res ? 405 : 404, …)`, so with `auth: "disabled"` any `/v1/workspaces/...`
  path answers **405 `method_not_allowed`**, not 404 (D9, Phase 2 last bullet, Security,
  acceptance criterion "new routes answer 404") → Fix: either add an explicit 404 for
  `res === "workspaces"` in `route()` with an `api.test.ts` case, or change D9 and the criterion
  to 405. Both are fail-closed. The first keeps the documented contract.
- **Critical: the child-environment recipe double-prefixes secrets.** Evidence: Phase 3 step 6
  says `NURTUO_SECRET_<EnvVar(name)>=<value>`. `secrets.EnvVar` already returns `NURTUO_SECRET_` +
  the mapped name (`env.go:26-37`), so a literal port produces `NURTUO_SECRET_NURTUO_SECRET_…` and
  every run fails `secret_missing` in the child → Fix: write `${envVar(name)}=<value>`, where
  `envVar` is a TS port of `EnvVar` that includes the prefix. Pin it with a shared fixture
  (`a-b`, `a_b`, `A.b`, digits), which `secret_name_collision` needs anyway.
- **Critical: the run-result mapping relies on CLI output that does not exist.** Evidence (PR #51
  `flowrun.go`): a timeout is `execErr("timed out after %s")`, which is code `execution`, not
  `timeout`. A Stop is `{error:{code:"stopped", message:"flow stopped with status N[: msg]"}}`,
  with no structured `stop.status`. Host-key mismatch and wrong password are raised by
  `connhost.Manager.Prepare` during the strict connection `Test` (`manager.go:181-227`) as
  `connection "<id>" (sftp): test failed: ssh connect: … host key mismatch: got …, want …`. That is
  before `eng.Run`, so under D16 these errors carry **no `steps`**. The failure table and two
  acceptance criteria ("timeout … code `timeout`", "host key does not match … connector step is
  `error`") depend on these shapes → Fix: extend D16 additively. Add `stop: {stepId, status,
  message}` and a distinct `timeout` code (or say that the control plane classifies timeouts by its
  own deadline). Add a structured `connection` (and `stepIds`) field to the JSON error for
  `Prepare` failures, so the canvas does not have to parse messages. Add CLI tests for each.

### Warning

- **Warning: the CI fingerprint and the TOFU scan will not match the key the connector
  negotiates.** Evidence: `connectors/sftp/config.go` sets no `HostKeyAlgorithms`. x/crypto
  v0.59.0 `defaultHostKeyAlgos` prefers ECDSA, then RSA, then ED25519 (`ssh/common.go:179-193`).
  `atmoz/sftp` has all three keys. PR #51 commit `dfb0946` already hit "knownhosts: key mismatch"
  for this reason. The spec computes `_FINGERPRINT` with `ssh-keyscan -t ed25519`, so the positive
  full e2e fails. A scan that "shows the SHA-256 fingerprint(s)" of several types invites the user to
  trust a key the connector never receives, which makes every run fail and conflicts with the D4 "no
  re-prompt" rule → Fix: `nurtuo flow host-key` dials with the connector's exact `ClientConfig`
  defaults and reports the negotiated key as the one to trust. Other types are shown as
  informational only, if at all. CI derives the fingerprint from that subcommand, or from
  `ssh-keyscan -t ecdsa`, with a comment.
- **Warning: D6 contradicts itself for the `runtime` role.** Evidence: "`connection:read` (all
  roles that have `component:read`)" includes `runtime` (`permissions.ts`: runtime holds
  `component:read`), but the same row says "`runtime` gets none" → Fix: list the roles explicitly
  (owner, admin, editor, viewer, deployer) and add a `rbac.test.ts` assertion that `runtime` lacks
  every `connection:*`, `secret:*` and `test_run:*` permission.
- **Warning: killing the child on client abort has no plumbing.** Evidence: `ApiRequest` has no
  signal, `createHandler`/`createAuthHook` pass none, and `EndpointCtx` has none (`api.ts`,
  `endpoints.ts`). Yet `TestRunner.run(ws, request, signal)` and an acceptance criterion depend on
  one → Fix: add `signal?: AbortSignal` to `ApiRequest` (set from `rq`/`rs` `close` in
  `createHttpServer`) and to `EndpointCtx`. Test it in `runner.test.ts` and `routes.test.ts`.
- **Warning: the child's disk and memory use is not bounded.** Evidence: the engine spools a GET
  payload into `TMPDIR`, and `writeOutput` calls `p.Bytes()` on every returned document before
  writing stdout (PR #51 `flowrun.go`). A 4 GiB remote file therefore fills the temp disk and child
  memory before the 1.5 MiB stdout cap can trigger. Gap #7 says "bounded by `TMPDIR=<tmp>`", but
  that only moves the files → Fix: add an additive `--max-output-bytes` (or `--max-payload`) to D16,
  enforced while spooling or returning, and map it to `output_too_large`. At minimum, record the
  gap honestly with a severity.
- **Warning: the from-scratch child environment ignores Windows.** Evidence: standalone targets
  Windows (`docs/ARCHITECTURE.md` e2e row, connections-and-secrets gap 1). The spec passes only
  `PATH`, `HOME` and `TMPDIR`. Go networking on Windows needs `SystemRoot`, and 0700/0600 modes
  have no effect on Windows → Fix: per-OS allowlist (`SystemRoot`/`SYSTEMROOT`, `TEMP`/`TMP`,
  `USERPROFILE` pointed at the temp dir), and a Windows note in Security. Or declare Windows test
  runs out of scope for this slice.
- **Warning: the restart acceptance criterion holds only with Postgres.** Evidence: `main.ts` uses
  `MemoryRepository` unless `DATABASE_URL` is set, and the spec adds `MemoryConnectionRepository`.
  Without `DATABASE_URL`, connections are also lost on restart → Fix: qualify the criterion with
  `DATABASE_URL` set, and add the memory-mode outcome.
- **Warning: #54 open widens who can trigger outbound connections.** Evidence: issue #54. Any
  session user can `POST /v1/workspaces`, becomes `owner` and so gets `test_run:execute` and
  `connection:write`, which includes host-key scans to any public host and port. Concurrency limits
  are specified for test runs only, not for `host-key-scan` → Fix: apply the semaphore (or a
  per-user rate limit) to `host-key-scan` too, and make D8's answer state that #54 being open is
  accepted for this endpoint, or require #54 first.
- **Warning: the egress denylist is incomplete.** Evidence: Phase 3 step 4 and `egress.test.ts`
  list loopback, RFC 1918, RFC 4193, link-local and `169.254.169.254`. Tests omit `0.0.0.0/8`,
  `::1`, IPv4-mapped `::ffff:0:0/96`, `fe80::/10`, `100.64.0.0/10` and `fd00:ec2::254` → Fix:
  write out the full list and test each entry. Also note that the Gaps #3 mitigation ("replace
  `host` by the checked IP") would break `known_hosts` hostname matching (fingerprint pinning is
  unaffected).
- **Warning: the compatibility classification is not reconciled with the contract.** Evidence: the
  [compatibility contract](../2026-11-22-backward-compatibility-contract.md) classes `nurtuo`
  subcommands/flags and control-plane environment variables as STABLE. The spec marks
  `nurtuo flow host-key`, `--json` `steps`, `NURTUO_FLOW_RUN_BIN` and
  `NURTUO_TEST_RUN_ALLOW_PRIVATE` "experimental" without saying so. The new 413 applies to every
  STABLE `/v1/` path → Fix: in Compatibility, state that these are EXPERIMENTAL by exception (the
  `nurtuo flow` subtree is already experimental per the `nurtuo flow run` spec) and label them in
  `docs/cli.md` / `docs/operations.md`. Document `413` in `openapi.yaml`. Note that control-plane
  gap 2 proposed 4 MiB, not 1 MiB.
- **Warning: dependency status is stale.** Evidence: PR #50 merged to `main` as `d5adeeb`, so
  `docs/specs/2026-11-23-nurtuoctl-run.md` is on `main`, but not on this branch, which is based on
  `ad72ae2` → Fix: rebase on `main`, link the spec, and update the Status table, the Drift row and
  Phase 1 (the changelog line now goes to the file on `main`).

### Important (gaps)

- **Important: the permission drift test regex rejects `test_run`.** Evidence: `crypto.test.ts:90`
  matches permission rows with `\('([a-z]+:[a-z_]+)', '[a-z]+', '[a-z_]+'\)`. The resource part
  `[a-z]+` does not match `test_run`, so the `0104` rows are not seen → Fix: widen to `[a-z_]+` in
  step 4, or name the resource `testrun`. Also update `ROLE_DESCRIPTIONS.editor`, which still reads
  "Create and edit components and revisions".
- **Important: the connection form cannot be generated "with `fieldsOf`" as described.** Evidence:
  `formSchema.ts` `Field` has no `writeOnly`, ignores `oneOf` (password | privateKey), and would
  render the `hostKeyPolicy` enum including `insecure` → Fix: extend `Field` with `writeOnly`, and
  state that the form filters `insecure` and offers password auth only in this slice (or handle
  `privateKey`/`passphrase` secrets as `<id>-privateKey` and `<id>-passphrase`).
- **Important: step status `stopped` is undefined.** Evidence: `Canvas.tsx` and `runState.test.ts`
  use `ok|error|not_run|stopped`, but Phase 3 step 8 defines only `ok`, `error` and `not_run` →
  Fix: define it (for example, the Stop step that ended the run is `stopped`, the steps after it are
  `not_run`), or drop it.
- **Important: connector steps without `connection` are unspecified.** Evidence: the flow format
  allows an empty `connection`, and the CLI then picks the only connection of that connector
  (`crossCheck`). The control plane writes only the referenced connections → Fix: require an
  explicit `connection` in test runs (400 `connection_required`), or define the default rule.
- **Important: no changeset for `@nurtuo/client`.** Evidence: AGENTS.md "Apache-2.0 packages that
  users consume need a changeset". Step 9 regenerates `@nurtuo/client` types → Fix: add
  `pnpm changeset` to step 9.
- **Important: the canvas serving model outside `vite dev` is unspecified.** Evidence: the same-origin
  cookie (`Secure; SameSite=Strict`, `auth/service.ts:118`) relies on the Vite proxy, and the
  control plane serves no static canvas → Fix: state that this slice is dev-only (`make dev`,
  Playwright) as a non-goal, or name the production topology.
- **Important: the CSRF token is lost on reload.** Evidence: the spec keeps the token from the
  login response in memory only. After a reload the session cookie is still valid, but mutations
  fail with `csrf`. `GET /v1/auth/me` already returns `csrfToken` (`endpoints.ts:78`) → Fix: restore
  it from `/v1/auth/me` on start.
- **Important: build-time schema reading is unclear.** Evidence: `@nurtuo/control-plane` `files`
  ships only `dist`, `migrations` and `openapi.yaml` → Fix: copy or embed
  `connectors/sftp/schemas/connection.json` into `dist`, and say how the copy is kept in sync (test
  against the source file).
- **Important: deleting a workspace leaves its secrets behind.** Evidence: the Pg `connections`
  rows cascade, but `MemorySecretStore`/`MemoryConnectionRepository` have no hook into
  `workspace.delete` → Fix: purge per workspace in the delete handler and test it.

### Rule compliance

- Ask First (AGENTS.md): `openapi.yaml`, CLI flags/subcommand and CI changes are acknowledged; the
  spec treats approval as the request. No new production dependency (`@nurtuo/flow-validate` is
  already a control-plane dependency; `golang.org/x/crypto` becomes direct in `core/runtime` via
  PR #51). License placement (all new code under `core/`) is correct.
- Template: every required section is present (Goal, Scope/Non-goals, Design, Step plan,
  Acceptance criteria, Tests, Contracts, Compatibility, Security, Gaps, Changelog). Every step names
  a verification.

## Factual errors in the spec

| # | Location in the spec | Spec says | Correct value / code |
|---|---|---|---|
| F1 | D11 row, Q11, Phase 3 step 2, Security "Egress", acceptance criterion "operation other than GET/LIST", Gaps #2 | SFTP read-only operations `GET` and `LIST` | SFTP has no `LIST`; listing is `QUERY` (`connectors/sftp/connector.yaml`, `sftp.go:55-68`). `engine.operationType` upper-cases the operation and maps unknown names to `EXECUTE` (`core/runtime/internal/engine/steps.go:278-292`) |
| F2 | D9 row, Phase 2 last bullet ("the legacy `route` returns 404"), Security "Authorization", acceptance criterion "`NURTUO_INSECURE_NO_AUTH=1` THEN … 404" | 404 | `route()` throws `405 method_not_allowed` for any path whose second segment is set (`core/control-plane/src/api.ts`, last line of `route`) |
| F3 | Phase 3 step 6 | `NURTUO_SECRET_<EnvVar(name)>=<value>` | `secrets.EnvVar(name)` already returns `NURTUO_SECRET_` + mapped name (`core/runtime/internal/secrets/env.go:26-37`); the variable is `EnvVar(name)` |
| F4 | Design table, SFTP row ("`GET` emits `<step>.out`"); response example `"id": "connector-1.out"`, properties `{path, filename}` | output id `<step>.out` | id is `<input document id>.out` (`sftp.go:584`), so `flow-run.out` under `nurtuo flow run` (injected doc `docs.New("flow-run", …)`); properties are the input properties merged with `path`, `filename` and `tracked.path`, `tracked.size` (`engine/steps.go:206-236`) |
| F5 | Response example, `returndocuments-1` | `"out": 0` | `Out = ok.Len()` for `returnDocuments` (`interp.go:203-208`), so `1` |
| F6 | Failure table (timeout, host key), Phase 3 step 8 (`stop.status`), acceptance criterion "code `timeout`" | CLI yields code `timeout` and a stop status | PR #51 `flowrun.go`: timeout is code `execution`, message `timed out after <d>`; Stop is `{error:{code:"stopped", message:"flow stopped with status N[: msg]"}}` with no structured status |
| F7 | Example failed message after the response; failure table "host key changed" | `connection "rebex-demo": host key mismatch: got …, want …` (implies an engine-time error) | raised in `connhost.Manager.Prepare` strict Test (`manager.go:214-224`): `connection "rebex-demo" (sftp): test failed: ssh connect: … host key mismatch: got …, want …`, before `eng.Run`, so no trace. The same applies to a wrong password |
| F8 | D6 row | `connection:read` for "all roles that have `component:read`" and "`runtime` gets none" | `runtime` holds `component:read` (`core/control-plane/src/auth/permissions.ts`); the two statements conflict |
| F9 | Phase 2, first bullet | "slugs can change via `PATCH /v1/workspaces/{ws}`" | `workspace.update` changes only `name` (`auth/endpoints.ts`; `MemoryAuthRepository.updateWorkspace(id, {name})`). Keying by id is still right, for future-proofing |
| F10 | Compatibility, "Roles" | "an editor can still grant editor" | editors lack `member:manage`, so they cannot add or modify members at all; `canGrant` only matters for owner/admin (`permissions.ts`, `endpoints.ts` `member.add`) |
| F11 | Header `Extends:`, Status table "none merged", Drift row for PR #50, Phase 1 ("its file is on PR #50") | PR #50 unmerged | PR #50 merged to `main` as `d5adeeb` (2026-11-23T17:14Z); `docs/specs/2026-11-23-nurtuoctl-run.md` is on `main` (Status: approved) |
| F12 | Design table, PR #51 row | `loadFlow` (`flow.ValidateJSON`+`flow.Parse`); `chainProvider{secrets.NewEnv(), secrets.NewDir()}` | `loadFlow` calls `flow.Parse`, which runs `ValidateJSON` itself; `secrets.NewDir` is added only when `--secrets-dir` is set |
| F13 | Design table, SFTP row (settings list) | omits a setting | schema also has `maxListEntries` (`connectors/sftp/schemas/connection.json`) |
| F14 | Goal ("ARCHITECTURE.md §10"), References ("§6.3 and §10 (Epic 10)") | Epic 10 in §10 | §10 is "Versioning and branches"; the epic table (row 10, Canvas) is in §11 "What to create first" (`docs/ARCHITECTURE.md:419-443`) |
| F15 | Gaps #7 | "`docs.Spool` disk use in the child bounded by `TMPDIR=<tmp>`" | `TMPDIR` only moves the spool; nothing bounds its size, and `writeOutput` loads each payload fully with `p.Bytes()` |

## What would unblock approval

Łukasz must answer these Decisions-table rows (one comment on the spec PR is enough):

- **Blocking per the spec:** D1 (sync vs async), D3 (in-memory workspace secret store), D4 (TOFU +
  paste, no insecure), D6 (permission set and roles; answer with the `runtime` role corrected, F8),
  D7 (spawn `nurtuo flow run` per run), D8 (path-scoped routes, #54/#55 not required; say
  explicitly whether #54 being open is accepted for host-key scans), D9 (login + same-origin
  proxy; 404 vs 405 in no-auth mode, F2), D10 (inline unsaved flow only), D11 (`sftp` read-only
  allowlist, corrected to `GET`/`QUERY`, F1), D16 (additive `steps`; extend to `stop`, a `timeout`
  code and a structured `connection` field, F6/F7).
- **Required by the readiness gate:** D14 (one spec with phases vs a split). The spec marks it
  non-blocking.
- **Recommended in the same comment** (non-blocking defaults, so they stop being assumptions): D2
  (limits), D5 (no history), D12 (env handoff; Windows variables), D13 (`atmoz/sftp` in CI; ECDSA
  fingerprint), D15 (path as an input property).

## During implementation

Not applicable: the verdict is `no-go`. Carry the Warning and Important items above into the
revised step plan.

## Next action

- Revise the spec with the spec-writing skill. Rebase on `main` (PR #50), fix F1–F15, add the
  Warning/Important fixes, and get Łukasz's answers to the rows above. Then set `approved` and
  re-run pre-implement-spec.

Readiness: no-go
Report: docs/specs/analysis/pre-implement-canvas-run.md
Spec: docs/specs/2026-11-23-canvas-run.md
