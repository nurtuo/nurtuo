<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Gate `POST /v1/workspaces` behind `instance:workspace_create`

Status: draft. Date: 2026-11-24.

Issue: https://github.com/nurtuo/nurtuo/issues/54 (phase 1 of epic https://github.com/nurtuo/nurtuo/issues/66).

Extends: [workspace-isolation](2026-11-22-workspace-isolation.md) (section "Workspace creation", implementation phase 1). Related: [auth-rbac-audit](implemented/2026-11-21-auth-rbac-audit.md) (gap 1, critical).

## TLDR

Today any session user can call `POST /v1/workspaces`, becomes `owner` of the new workspace and, because
control-plane data is not partitioned yet, gains every data permission over every existing row. This spec
adds one instance-level permission, `instance:workspace_create`, held only by an env-configured
instance-admin set plus the bootstrap user, and makes `POST /v1/workspaces` require it. The default
(`NURTUO_ALLOW_WORKSPACE_CREATE=admins`) closes the escalation; `any` restores today's behaviour. Data
partitioning (#55) is not part of this spec. Several defaults below change who may create workspaces and
are **pending owner confirmation** (see [Decisions](#decisions)).

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/72 (this spec, docs only); implementation will use `Closes #54` on a later `feat/` PR after approval |
| ADRs | none for phase 1 (decision D1: no ADR until a DB-backed instance role model is designed); the parent's planned ADR "workspace is the tenant boundary" belongs to phase 2 |
| Issues | https://github.com/nurtuo/nurtuo/issues/54 (this spec), epic https://github.com/nurtuo/nurtuo/issues/66, follow-up https://github.com/nurtuo/nurtuo/issues/55 (phase 2) |
| Code | `core/control-plane/src/auth/{policy,middleware,endpoints,service,permissions,authz,test-util}.ts`, new `core/control-plane/src/auth/instance.ts`, `core/control-plane/src/main.ts`, `core/control-plane/openapi.yaml` (description only) |
| User docs | [auth.md](../auth.md), [security.md](../security.md), [operations.md](../operations.md) (standalone bootstrap owner) |
| Pre-implement report | [analysis/pre-implement-workspace-create-gate.md](analysis/pre-implement-workspace-create-gate.md) |

## Goal

Operators of a shared control plane need ordinary users (any role in any workspace) to be unable to mint
themselves a workspace and, through it, full data access. Today `POST /v1/workspaces` is `access:
"authenticated"`, so they can. Proposed: an instance-level permission checked by the auth hook before
`workspace.create` runs, granted from an operator-controlled list, defaulting to "admins only". This is the
smallest change that closes `auth-rbac-audit` gap 1 for fresh installs and can ship before the data-plane
migration (#55).

## Open Questions

Blocking questions are answered by a recommendation in [Decisions](#decisions) and stay open until Łukasz
confirms them. The Design below uses the recommended (lowest blast radius) answer and marks it *pending*.

- **Q1 (blocking, D1):** Is the phase-1 instance-admin set `NURTUO_INSTANCE_ADMINS` (comma-separated emails)
  plus the bootstrap email, checked per request by the caller's email? Or must phase 1 already be DB-backed?
- **Q2 (blocking, D2):** Default after upgrade: `admins` (ordinary session users lose `POST /v1/workspaces`)
  or `any` (today's behaviour, opt-in tightening)?
- **Q3 (blocking, D3):** Is "the bootstrap user" the account whose email is `NURTUO_BOOTSTRAP_EMAIL`,
  whenever that variable is set, independent of `NURTUO_BOOTSTRAP_PASSWORD`?
- **Q4 (blocking, D9):** May phase 1 also change `POST /v1/workspaces/{ws}/members` so that a caller who is
  not an instance admin cannot *create* a new account whose email is in the instance-admin set (otherwise
  a workspace admin can pre-register an admin email and take over the instance-admin grant)?
- **Q5 (blocking, D12):** Workspaces that ordinary users already created before the upgrade keep their
  owners (and therefore full data access until #55). Is documenting an audit query in the release note
  enough, or must the upgrade revoke something?
- **Q6 (non-blocking):** The parent's gap 4 says the DB-backed `instance_roles` table is "phase 2", but
  #55 (phase 2) does not list it. Which issue owns it? Recommendation: a separate follow-up issue, not #55.
  Does not block phase 1.

## Scope

In scope (issue #54 only, one shippable slice):

- A new instance-level permission `instance:workspace_create`, represented outside the per-workspace role
  matrix.
- Instance-admin set from `NURTUO_INSTANCE_ADMINS` plus the bootstrap email; mode
  `NURTUO_ALLOW_WORKSPACE_CREATE` (`admins` default, `any`).
- `POST /v1/workspaces` requires `instance:workspace_create`; denial is 403 `forbidden` and audited.
- Guard in `member.add` against pre-registering an instance-admin email (pending D9).
- Startup validation and log lines in `main.ts`.
- Tests in the same PR; docs (`auth.md`, `security.md`, `openapi.yaml` description).

Non-goals:

- Data partitioning: migration `0200_workspace_isolation.sql`, `Repository` signatures taking `ws`,
  `route(repo, req, ctx)`, `GatewayRepository`, per-workspace component names. All of that is #55 (parent
  phase 2). This spec does not touch `core/control-plane/migrations/`, `repository.ts`,
  `memory-repository.ts` (data), `pg-repository.ts` (data), `api.ts` routing or `gateway.ts`.
- Removing the "not partitioned by workspace" Known gap in `auth.md`: it stays until #55 ships.
- A DB-backed instance role table (`instance_roles`), instance-admin management API, instance-level audit
  endpoint, or an `instance:admin` role with more permissions (Q6, follow-up).
- Gating other workspace-lifecycle routes (D10): `PATCH`/`DELETE /v1/workspaces/{ws}` and member routes keep
  their per-workspace permissions.
- Instance-scoped API tokens: `POST /v1/workspaces` stays session-only (D7).
- Exposing instance permissions in `GET /v1/auth/me` or `GET /v1/roles` (D13).
- Row-level security and path-scoped aliases (parent phase 3).
- **Windows DACL on the standalone secrets directory (#57).** That is a separate security topic sourced from
  [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md) gap 1 (`checkPerm`,
  `chmodPrivate`, `%ProgramData%\Nurtuo`). It does not depend on this spec and this spec does not block it.

## Design

### Current behavior (as the code works on `main`, commit `7953d4f`)

- **Route policy.** `core/control-plane/src/auth/policy.ts:81`:
  `P("POST", "/v1/workspaces", authed("workspace", "workspace.create", { userOnly: true }))`. `RoutePolicy.access`
  is `"public" | "authenticated" | "permission"` (`policy.ts:14`); `"authenticated"` means any valid
  principal; `workspace: "none"`.
- **Middleware.** `createAuthHook` in `src/auth/middleware.ts`: match policy, authenticate, refuse tokens on
  `userOnly` routes (`middleware.ts:143`, 403 `forbidden` "this endpoint requires a user session"), CSRF for
  cookie sessions, resolve workspace (skipped for `"none"`), `authorize()` only when `access === "permission"`
  (`middleware.ts:155`), run handler, audit every mutating request (including denials).
- **Handler.** `endpoints["workspace.create"]` (`src/auth/endpoints.ts:135`) validates `slug` / `name` and
  calls `repo.createWorkspace({ slug, name, ownerId: p.userId })`; `MemoryAuthRepository.createWorkspace`
  (`src/auth/memory-repository.ts:44`) and `PgAuthRepository.createWorkspace` (`src/auth/pg-repository.ts:59`)
  insert the creator as `owner`.
- **Permission model is per workspace only.** `RESOURCES` (`src/auth/permissions.ts:6`) has no instance
  resource; the `Permission` type is `` `${Resource}:${string}` ``; `ROLE_PERMISSIONS.owner = ALL_PERMISSIONS`
  (`permissions.ts:32`); `permissionsIn(p, workspaceId)` (`src/auth/authz.ts:11`) returns permissions inside
  one workspace. `Principal` (`src/auth/types.ts`) carries `userId` and `memberships`, not the email. There
  is no instance-level permission, role, table or config anywhere in `core/control-plane` (searched for
  `instance:`, `NURTUO_INSTANCE_ADMINS`, `NURTUO_ALLOW_WORKSPACE_CREATE`: no matches outside the parent spec).
- **Bootstrap.** `src/main.ts:41`: when both `NURTUO_BOOTSTRAP_EMAIL` and `NURTUO_BOOTSTRAP_PASSWORD` are set,
  `AuthService.bootstrap(email, password)` (`src/auth/service.ts:76`) gets or creates the user and gets or
  creates workspace `default` (owner = that user only when the workspace is created now). It is idempotent
  and runs on every start while both variables are set.
- **Standalone.** `core/runtime/cmd/nurtuo/standalone.go` (`controlPlaneEnv`) always passes
  `NURTUO_BOOTSTRAP_EMAIL=admin@nurtuo.local` (`standaloneAdminEmail`, `standalone_enroll.go:20`) and
  `NURTUO_BOOTSTRAP_PASSWORD=<admin token>` to the control-plane child.
- **Auth disabled.** With `NURTUO_INSECURE_NO_AUTH=1`, `createHandler` calls `route()` directly; `route()` has
  no `workspaces` branch and ends with `throw new ApiError(res ? 405 : 404, ...)` (`src/api.ts:166`), so
  `POST /v1/workspaces` answers 405 `method_not_allowed`. Workspace creation is not reachable without auth.
- **Users.** Emails are stored lower-case and unique (`migrations/0100_auth_identity.sql`,
  `users_email_lower`, `users_email_key`). `member.add` (`endpoints.ts:162`) creates a new local account with an
  admin-chosen password when the email is unknown (`endpoints.ts:172`); it never changes an existing
  account's password. OIDC JIT (`endpoints.ts:109`) can create accounts, but `main.ts` does not wire OIDC.
- **Clients.** No first-party client calls `POST /v1/workspaces`: searched `sdk/`, `cli/`, `core/web`,
  `core/mcp`, `core/runtime` (`.ts`, `.tsx`, `.go`); only `core/control-plane/src/auth/*` and its tests
  reference it. The existing test `workspace creation is for session users only; creator owns it`
  (`src/auth/rbac.test.ts:183`) creates a workspace as a workspace `admin` with no instance role.
- **OpenAPI.** `openapi.yaml` `createWorkspace` already declares `403` (`#/components/responses/Forbidden`);
  description: "Creates a workspace; the caller becomes its owner. Session users only."

### Drift in prior specs (design is against the code)

| Path | Spec says | Code does | Treatment |
|---|---|---|---|
| [workspace-isolation](2026-11-22-workspace-isolation.md) "Workspace creation" | permission "held by members of an instance-admin list" | there is no instance membership concept; `Principal.memberships` are workspace memberships only | this spec defines the set by email match (D1); wording only |
| workspace-isolation, Testing | `isolation: workspace creation needs instance:workspace_create` lives in `src/auth/isolation.test.ts` | file does not exist; workspace-create tests live in `src/auth/rbac.test.ts` | phase-1 tests go into `rbac.test.ts` and a new `instance.test.ts`; `isolation.test.ts` is created by #55 |
| workspace-isolation, Gaps #4 | `instance_roles` table is "phase 2" | phase 2 (#55) does not list it | Q6, non-blocking |
| workspace-isolation, Request path | `auth: "disabled"` uses `X-Nurtuo-Workspace` / `default` | that is a phase-2 change; today `POST /v1/workspaces` is 405 in disabled mode (`api.ts:166`) | nothing to do in phase 1 (D8) |
| [auth.md](../auth.md) `main.ts` environment | bootstrap "creates the first owner and workspace `default`" | `bootstrap` reuses an existing `default` workspace without adding the bootstrap user as member (`service.ts:76`) | not changed here; noted because "bootstrap user" in this spec means the email, not "owner of `default`" (D3) |
| epic #66 body | "(approved 2026-11-23)" | parent spec still `Status: draft` | planning note, not spec approval; this spec stays `draft` |

### Proposed approach (pending D1 to D4, D9)

**1. Instance permissions live beside, not inside, the workspace matrix (D5).** New file
`src/auth/instance.ts` (AGPL, SPDX header):

```ts
export const INSTANCE_PERMISSIONS = ["instance:workspace_create"] as const;
export type InstancePermission = (typeof INSTANCE_PERMISSIONS)[number];
export type WorkspaceCreateMode = "admins" | "any";
export interface InstanceConfig { admins: ReadonlySet<string> /* lower-case emails */; workspaceCreate: WorkspaceCreateMode }
export function parseInstanceConfig(env: Record<string, string | undefined>): InstanceConfig; // throws InstanceConfigError
export function instancePermissionsFor(email: string | undefined, cfg: InstanceConfig): ReadonlySet<InstancePermission>;
```

It is deliberately **not** added to `RESOURCES` / `ACTIONS` in `permissions.ts`, because
`ROLE_PERMISSIONS.owner = ALL_PERMISSIONS` would hand it to every workspace owner and admin (the very
escalation this closes), `isPermission` would accept it as an API-token scope, `GET /v1/roles` would list
it, and `migrations/0103_auth_seed_roles.sql` plus its drift test would need a migration. Keeping it a
separate type keeps the workspace matrix, token scopes and seed data unchanged.

**2. Config parsing.** `parseInstanceConfig(env)`:

- `NURTUO_INSTANCE_ADMINS`: split on `,`, trim, drop empty items, lower-case; each item must pass the same
  check as `AuthService.validateEmail` (the regex is reused, not duplicated). A malformed item throws
  `InstanceConfigError` naming the variable and the 1-based item position (D4). Unset or empty = no
  listed admins.
- `NURTUO_BOOTSTRAP_EMAIL`, when set and non-empty, is added to the set after the same normalisation (D3).
  Malformed bootstrap email also throws.
- `NURTUO_ALLOW_WORKSPACE_CREATE`: unset or empty = `admins` (D2, pending); `admins` or `any`
  (case-insensitive, trimmed); anything else throws.

`instancePermissionsFor(email, cfg)`: mode `any` returns `{instance:workspace_create}` for every caller;
mode `admins` returns it only when `email` (lower-cased) is in `cfg.admins`; else empty.

**3. Service wiring.** `AuthServiceOptions` gains optional `instance?: InstanceConfig`. When omitted the
service uses `{ admins: new Set(), workspaceCreate: "admins" }` (fail closed for embedders of
`createAuthHook` and for `setup()` in tests). New method:

```ts
async instancePermissions(p: Principal): Promise<ReadonlySet<InstancePermission>>
```

It returns empty for `p.kind !== "user"`; otherwise it reads the user with `repo.getUserById(p.userId)` and
calls `instancePermissionsFor(user?.email, cfg)`. The lookup is per request and only on routes that need it
(today one route), so a newly created account with an admin email is recognised without a restart and a
removed env entry takes effect on restart. `authenticateJwt` already rejects disabled users before this runs.

**4. Policy and middleware.** `RoutePolicy.access` gains `"instance"`; `RoutePolicy` gains
`instancePermission?: InstancePermission`. New helper `inst(permission, resource, action, extra)` with
`workspace: "none"`. The table row becomes:

```ts
P("POST", "/v1/workspaces", inst("instance:workspace_create", "workspace", "workspace.create", { userOnly: true })),
```

In `createAuthHook.inner`, after the `userOnly` and CSRF checks and before `run(ws)`:

```ts
if (policy.access === "instance") {
  if (!(await service.instancePermissions(principal)).has(policy.instancePermission!)) {
    if (mutating) await safeAudit(() => audit(req, { ...auditBase, status: 403, extra: { denied: "forbidden", permission: policy.instancePermission } }));
    return errRes(403, "forbidden", `missing permission ${policy.instancePermission}`);
  }
}
```

Order of checks for `POST /v1/workspaces` therefore stays: 401 anonymous, 403 token (`userOnly`),
403 `csrf`, then 403 `forbidden` (instance permission), then 400/409 from the handler. Body validation is not
reachable without the permission, so slug existence is not probeable by non-admins. The handler
`workspace.create` is unchanged; the creator still becomes `owner`.

**5. Pre-registration guard (pending D9).** In `endpoints["member.add"]`, in the branch where no account with
that email exists and a new one is about to be created: if the email is in the instance-admin set
(`cfg.admins`, mode-independent) and `instancePermissions(caller)` does not contain
`instance:workspace_create`, throw `forbidden("role_escalation", "only an instance admin can create an account for an instance-admin email")`.
Adding an *existing* account as a member is unchanged (it does not change who owns the email). Without this
guard, an owner or admin of any workspace could create `alice@corp.example` (listed in
`NURTUO_INSTANCE_ADMINS` but not yet registered) with a password they choose, log in and create
workspaces. `AuthService` exposes `isInstanceAdminEmail(email)` for this check.

**6. `main.ts`.** Before any database work, `parseInstanceConfig(process.env)`; on `InstanceConfigError`
print `console.error(<message>)` and `process.exit(1)` (same style as the `NURTUO_AUTH_SECRET` checks). Skip
parsing when `NURTUO_INSECURE_NO_AUTH=1` (no auth hook, route unreachable). Pass `instance` to
`new AuthService(authRepo, { secret, instance })`. Startup log lines:

- mode `admins`, empty set: `console.warn("no instance admins configured (NURTUO_INSTANCE_ADMINS, NURTUO_BOOTSTRAP_EMAIL): POST /v1/workspaces is denied for everyone")`.
- mode `admins`, non-empty: `console.log("instance admins: <n> configured")` (count only).
- mode `any`: `console.warn("NURTUO_ALLOW_WORKSPACE_CREATE=any: every session user can create a workspace and becomes its owner; control-plane data is not partitioned by workspace yet (#55)")`.

**7. Docs and OpenAPI.** `openapi.yaml` `createWorkspace` description only (no schema, path or response
change; `403` is already declared): "Requires the instance-level permission `instance:workspace_create`
(instance admins, see `NURTUO_INSTANCE_ADMINS` and `NURTUO_ALLOW_WORKSPACE_CREATE`). The caller becomes its
owner. Session users only." `auth.md`: env list, a short "Instance admins" section, request pipeline step for
instance permissions. `security.md`: the REST API row names the gate. `operations.md`: the standalone
paragraph notes that `admin@nurtuo.local` is the instance admin. The "not partitioned" Known gap stays.

### Alternatives considered

- **DB-backed `instance_roles` table + API** (parent gap 4): needs a migration, repository methods in both
  auth repositories, a management API (new public contract) and a bootstrap path. Larger than #54 and not
  needed to close the escalation; deferred (Q6, D1).
- **Treat owners of workspace `default` as instance admins:** needs no config, but `default` may not exist,
  its owners are a workspace-level grant that workspace owners can hand out (`member.add` with role `owner`
  by an owner), and `bootstrap` does not always make the bootstrap user its owner. Rejected.
- **Add `instance` to `RESOURCES`:** gives the permission to every workspace owner/admin through
  `ALL_PERMISSIONS`; rejected (see step 1).
- **Resolve the admin emails to user ids at startup:** misses accounts created after startup and still needs
  the email guard; rejected for per-request lookup.

### Failure modes and what the operator sees

| Situation | Result |
|---|---|
| `NURTUO_INSTANCE_ADMINS="ops@corp.example,not-an-email"` | control plane exits 1 at start: `NURTUO_INSTANCE_ADMINS item 2 is not a valid email` |
| `NURTUO_ALLOW_WORKSPACE_CREATE=yes` | exits 1: `NURTUO_ALLOW_WORKSPACE_CREATE must be "admins" or "any"` |
| No admins and no bootstrap email, mode `admins` | starts, warns; every `POST /v1/workspaces` is 403 `forbidden`; existing workspaces work |
| Listed admin email has no account yet | nothing happens until an instance admin creates it (`member.add`) or it logs in via a wired IdP |
| Operator removes `NURTUO_BOOTSTRAP_EMAIL` after first run and does not list it in `NURTUO_INSTANCE_ADMINS` | that user can no longer create workspaces after restart; documented in `auth.md` |
| Non-admin session user calls `POST /v1/workspaces` | 403 `{"error":{"code":"forbidden","message":"missing permission instance:workspace_create"}}`, one audit row (`workspace.create`, status 403, `workspace_id` null) |
| API token calls `POST /v1/workspaces` | 403 "this endpoint requires a user session" (unchanged), audited as today |

### Rollback

Reverting the implementation PR restores `authed(...)` and ignores the two env vars; no data or schema
change is involved. Without a revert, operators roll back behaviour with `NURTUO_ALLOW_WORKSPACE_CREATE=any`.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `POST /v1/workspaces` | requires `instance:workspace_create`; 403 `forbidden` otherwise. Path, method, request and response schemas unchanged; `403` already documented | experimental (behaviour change, D2) |
| `POST /v1/workspaces/{ws}/members` | new 403 `role_escalation` when creating a new account for an instance-admin email without being an instance admin (D9) | experimental |
| Environment `NURTUO_INSTANCE_ADMINS` | new; comma-separated emails | experimental (runtime config key: Ask First, covered by approving this spec) |
| Environment `NURTUO_ALLOW_WORKSPACE_CREATE` | new; `admins` (default) or `any` | experimental (as above) |
| Environment `NURTUO_BOOTSTRAP_EMAIL` | additionally makes that email an instance admin (additive meaning, D3) | experimental per [auth-rbac-audit](implemented/2026-11-21-auth-rbac-audit.md); STABLE per the draft [backward-compatibility contract](2026-11-22-backward-compatibility-contract.md) (`NURTUO_BOOTSTRAP_*`). The existing meaning (create user and `default`) is unchanged |
| Permission id `instance:workspace_create` | new; not a token scope, not in `GET /v1/roles`, not in `0103` seed | experimental |
| `openapi.yaml` `createWorkspace.description` | text only | n/a |
| `AuthServiceOptions.instance`, `AuthService.instancePermissions`, `AuthService.isInstanceAdminEmail`, `RoutePolicy.access: "instance"`, `src/auth/instance.ts` exports | new | internal (exported from `src/auth/index.ts` for embedders) |

No `proto/`, `nurtuo.flow/v1`, `nurtuo.bundle/v1`, CLI flag or MCP tool schema changes. No migration.
The draft backward-compatibility contract lists control-plane env as STABLE; the two new variables are
proposed as experimental until that contract is approved. They are new keys, but unset
`NURTUO_ALLOW_WORKSPACE_CREATE` means `admins`, which is the behaviour change decided in D2.

## Compatibility

- **Fresh installs, default mode:** only the bootstrap email and listed admins can create workspaces.
- **Standalone (`nurtuo standalone`):** the only interactive account is `admin@nurtuo.local`, always passed as
  `NURTUO_BOOTSTRAP_EMAIL`, so it keeps creating workspaces; no Go change in `core/runtime`.
- **Self-hosted, several users:** existing workspaces, memberships and tokens are untouched. Non-admin users
  get 403 on `POST /v1/workspaces` after upgrade. Migration: list them in `NURTUO_INSTANCE_ADMINS`, or set
  `NURTUO_ALLOW_WORKSPACE_CREATE=any` (keeps the escalation open; startup warning).
- **Clients:** no first-party client calls the route (see Current behavior). Third-party scripts that create
  workspaces with a non-admin session see 403, a status the OpenAPI already declares.
- **Is it breaking?** Under the draft [backward-compatibility contract](2026-11-22-backward-compatibility-contract.md),
  paths, methods, required fields and response meanings are unchanged, so it is not a schema break; it is an
  authorization tightening. Whether that needs the `needs-owner-review` merge gate is part of D2.
- **Workspaces created before the upgrade by ordinary users** keep their owners, who keep full data access
  until #55 (D12). The release note ships an audit query: `SELECT ts, actor_id, resource_id FROM audit_log
  WHERE action = 'workspace.create' AND status = 201 ORDER BY seq;` plus `SELECT w.slug, u.email FROM
  workspaces w JOIN memberships m ON m.workspace_id = w.id AND m.role = 'owner' JOIN users u ON u.id = m.user_id;`.
- **Downgrade:** safe; the variables are ignored by older versions.
- `core/control-plane` is AGPL and not a consumed package: no changeset; a release-note line is enough.

## Security

- Closes `auth-rbac-audit` gap 1 for the default configuration: a viewer of `default` cannot create a
  workspace, so cannot become an owner anywhere and cannot reach `component:delete` / `deployment:create`.
- Fail closed: unset mode = `admins`; omitted `AuthServiceOptions.instance` = no admins; malformed config =
  no start. The permission is never derived from a workspace role or token scope.
- Denials are audited with the existing pipeline. The row has `workspace_id = NULL`, so it is visible to DB
  operators but not through `GET /v1/workspaces/{ws}/audit` (existing gap 11 of `auth-rbac-audit`).
- Email-based identity is only as strong as account creation: the D9 guard closes local pre-registration;
  OIDC JIT is not wired in `main.ts` (risk 3 below).
- Admin emails are not secrets; the startup log prints only the count.
- `any` is an explicit, logged weakening chosen by the operator.

## Step plan

### Phase 1: gate workspace creation (the whole of #54; one PR)

1. **Characterization.** Run `rbac.test.ts` `workspace creation is for session users only; creator owns it`
   on unchanged code (green), and add a test `escalation (characterization): a viewer can create a workspace
   today` that is green on unchanged code. In the same PR this test is turned into
   `workspace create gate: a viewer of default cannot create a workspace (403) and gains no owner role` once
   step 4 lands. Verified: `pnpm --filter @nurtuo/control-plane run build && pnpm --filter @nurtuo/control-plane run test`.
2. **`src/auth/instance.ts`** with `INSTANCE_PERMISSIONS`, `parseInstanceConfig`, `instancePermissionsFor`,
   `InstanceConfigError`; export from `src/auth/index.ts`. No caller yet, system unchanged. Verified: new
   `src/auth/instance.test.ts`.
3. **`AuthService`**: `instance` option with fail-closed default, `instancePermissions(p)`,
   `isInstanceAdminEmail(email)`. No route uses it yet. Verified: `instance.test.ts` service cases.
4. **Policy + middleware**: `access: "instance"`, `instancePermission`, `inst()` helper, the `POST
   /v1/workspaces` row, the middleware check; update `policy.test.ts` `policy table is well-formed` to accept
   `"instance"` (must have a valid `instancePermission`, `workspace: "none"`, no `permission`); update the
   existing `rbac.test.ts` workspace-creation test to pass `setup({ service: { instance } })` with the
   caller as admin. Verified: `rbac.test.ts`, `policy.test.ts`, `authn.test.ts` green.
5. **`member.add` guard** (D9). Verified: `rbac.test.ts` guard test.
6. **`main.ts` wiring**: parse before DB work, exit 1 on error, log lines, pass `instance`. Verified:
   `src/main-config.test.ts` (spawns `dist/main.js`) and the manual check below.
7. **Docs**: `openapi.yaml` description, `auth.md`, `security.md`, `operations.md`; changelog line in this
   spec and status `in progress`/`implemented` as appropriate. Verified: `policy.test.ts` (OpenAPI still
   parses and matches the policy table), `node scripts/check-docs-links.mjs`.

Every step leaves `main` releasable: steps 2 and 3 add unused code; step 4 is the behaviour change and
ships with its tests and step 6 in the same PR (otherwise `main.ts` would run with "no admins").

## Acceptance criteria

- WHEN a session user who is `viewer` (or `editor`, `deployer`, `admin`, `owner`) of workspace `default`, and
  whose email is not an instance admin, calls `POST /v1/workspaces {slug:"mine",name:"m"}` with valid CSRF in
  the default configuration THEN the response is 403 `{"error":{"code":"forbidden","message":"missing permission instance:workspace_create"}}`,
  no workspace `mine` exists, the caller's memberships are unchanged, and one audit row with action
  `workspace.create`, status 403 and `metadata.permission = "instance:workspace_create"` exists.
- WHEN that same viewer then calls `DELETE /v1/components/{id}` with `X-Nurtuo-Workspace: mine` THEN the
  response is 403 (workspace not found / not a member) and the component still exists: the critical
  escalation (create a workspace and thereby gain every data permission over existing rows) is closed for
  the default configuration.
- WHEN the caller's email is listed in `NURTUO_INSTANCE_ADMINS` (any case, surrounding spaces) THEN
  `POST /v1/workspaces` returns 201 and the caller is `owner` of the new workspace.
- WHEN the caller's email equals `NURTUO_BOOTSTRAP_EMAIL` and `NURTUO_INSTANCE_ADMINS` is unset THEN
  `POST /v1/workspaces` returns 201.
- WHEN `NURTUO_ALLOW_WORKSPACE_CREATE=any` THEN any session user gets 201 (today's behaviour) and the control
  plane logged the `any` warning at start.
- WHEN an API token (any scopes, any role) calls `POST /v1/workspaces` in either mode THEN the response is 403
  "this endpoint requires a user session".
- WHEN a non-admin's request lacks the permission THEN slug validation and slug conflicts are not evaluated
  (an invalid or taken slug also returns 403, not 400/409).
- WHEN `NURTUO_INSTANCE_ADMINS` contains a malformed item or `NURTUO_ALLOW_WORKSPACE_CREATE` is not `admins`/`any`
  THEN the control plane exits with status 1 and an error naming the variable, before listening.
- WHEN mode is `admins` and the set is empty THEN the control plane starts, logs the "no instance admins"
  warning, and every `POST /v1/workspaces` is 403.
- WHEN a workspace `owner`/`admin` who is not an instance admin calls `POST /v1/workspaces/{ws}/members` with a
  new email that is in the instance-admin set THEN the response is 403 `role_escalation` and no account is
  created; WHEN an instance admin does the same THEN the account is created (201) (pending D9).
- WHEN `NURTUO_INSECURE_NO_AUTH=1` THEN `POST /v1/workspaces` is still 405 (unchanged) and the new variables are
  not parsed.
- WHEN `GET /v1/roles` is called THEN `permissions` does not contain `instance:workspace_create`; WHEN a token is
  minted with scope `instance:workspace_create` THEN the response is 400 `unknown scope`.

## Tests

All in `core/control-plane`, Node test runner on compiled output. Run:
`pnpm --filter @nurtuo/control-plane run build && pnpm --filter @nurtuo/control-plane run test`
(and `typecheck`). CI runs them in the existing TS job. No test needs `DATABASE_URL`; each test builds its own
in-memory fixture with `setup()` from `src/auth/test-util.ts` (fresh repositories per test, nothing to clean
up). `setup()` already accepts `service: Partial<AuthServiceOptions>`, so tests pass `instance` without
changing the helper; a small helper `adminsConfig(...emails)` may be added to `test-util.ts`.

| Test | File | What it proves |
|---|---|---|
| `instance config: defaults to admins with an empty set` | `src/auth/instance.test.ts` (new) | fail-closed default |
| `instance config: parses, trims, lower-cases and de-duplicates NURTUO_INSTANCE_ADMINS; ignores empty items` | same | parsing |
| `instance config: bootstrap email is an instance admin; malformed bootstrap email throws` | same | D3 |
| `instance config: malformed admin item throws with the item position; unknown mode throws` | same | D4 |
| `instance config: mode any grants instance:workspace_create to every email; admins only to listed ones` | same | `instancePermissionsFor` |
| `instance permissions: tokens never hold instance permissions; disabled users are rejected before the check` | same (uses `setup()`) | `AuthService.instancePermissions` |
| `instance permissions are not workspace permissions: not in ALL_PERMISSIONS, not a token scope, not in /v1/roles` | same | D5 isolation from the matrix |
| `escalation (characterization): a viewer can create a workspace today` → renamed in the same PR to `workspace create gate: a viewer of default cannot create a workspace (403) and gains no owner role` | `src/auth/rbac.test.ts` | issue #54 "non-admin cannot"; acceptance 1 |
| `workspace create gate: closes auth-rbac-audit gap 1 (viewer cannot delete a component through a self-created workspace)` | same | acceptance 2 |
| `workspace create gate: every workspace role without instance admin is denied` (loop over `ROLES`) | same | |
| `workspace create gate: an instance admin (listed, any case) can create and owns the workspace` | same | issue #54 "instance admin can" |
| `workspace create gate: the bootstrap email can create without NURTUO_INSTANCE_ADMINS` | same | D3 |
| `workspace create gate: mode any restores today's behaviour` | same | D2 escape hatch |
| `workspace create gate: tokens are refused in both modes` | same | D7 |
| `workspace create gate: denial precedes body validation (bad or taken slug is 403 for non-admins)` | same | no slug probing |
| `workspace create gate: denial is audited with permission metadata and null workspace` | same | D6 |
| `workspace creation is for session users only; creator owns it` (existing, updated to configure the caller as instance admin) | same | unchanged handler behaviour |
| `member add: a non-instance-admin cannot pre-register an instance-admin email; an instance admin can; adding an existing account is unchanged` | same | D9 |
| `policy table is well-formed` (existing, extended for `access: "instance"`) | `src/auth/policy.test.ts` | table invariants |
| `every openapi operation has an authz policy, and every policy is documented` (existing) | same | OpenAPI still in sync |
| `runtime: every non-public documented route rejects anonymous callers with 401 ...` (existing) | same | 401 before instance check |
| `auth disabled is explicit and keeps legacy behaviour` (existing) plus new assertion `POST /v1/workspaces` is 405 | `src/auth/authn.test.ts` | D8 |
| `main: invalid NURTUO_INSTANCE_ADMINS or NURTUO_ALLOW_WORKSPACE_CREATE exits 1 before listening` | `src/main-config.test.ts` (new) | spawns `process.execPath dist/main.js` with a 32-byte `NURTUO_AUTH_SECRET`, no `DATABASE_URL`, `PORT=0`; asserts exit code 1 and stderr names the variable; kills the child after a 10 s timeout |

Manual check (documented in the implementation PR): `NURTUO_AUTH_SECRET=<32+ chars> NURTUO_BOOTSTRAP_EMAIL=ops@example.test NURTUO_BOOTSTRAP_PASSWORD=<12+ chars> node dist/main.js`,
log in as `ops@example.test` and create a workspace (201); add `viewer@example.test` to `default` as viewer, log
in as viewer and create a workspace (403).

## Decisions

Recommendations only. Nothing here is approved. `needs-owner` rows block approval; `implementer-default`
rows are reversible engineering defaults that do not need to block approval.

| ID | Question | Options | Recommendation | Why | Class |
|---|---|---|---|---|---|
| D1 | How is the instance-admin set defined in phase 1? | (a) `NURTUO_INSTANCE_ADMINS` emails + bootstrap email, matched per request by the caller's email; (b) DB-backed `instance_roles` table + API; (c) owners of workspace `default` | (a); no ADR until (b) is designed in a follow-up | No migration or new API; reversible; matches the parent and #54; (c) is a workspace grant owners can hand out | needs-owner |
| D2 | Default after upgrade: who may create workspaces? | (a) `admins` default, `NURTUO_ALLOW_WORKSPACE_CREATE=any` escape hatch; (b) `any` default, opt-in `admins` | (a) | Only (a) closes the critical escalation without operator action; no first-party client uses the route; standalone keeps working via the bootstrap email; `403` already in OpenAPI. Behaviour change for multi-user self-hosted installs, mitigated by `any` and the release note. Owner also decides whether the implementation PR needs `needs-owner-review` | needs-owner |
| D3 | Who is the bootstrap user? | (a) the email in `NURTUO_BOOTSTRAP_EMAIL` whenever set; (b) only when bootstrap actually ran (email and password set); (c) the user `bootstrap` created first, persisted | (a) | Standalone always sets it; operators may drop the password after first run; no persistence needed. Removing the email removes the grant (documented) | needs-owner |
| D4 | Empty or malformed `NURTUO_INSTANCE_ADMINS` / unknown mode? | (a) malformed or unknown: exit 1; empty: start with warning, nobody can create; (b) ignore bad items with a warning | (a) | Fail loud and closed, like `NURTUO_AUTH_SECRET` checks; empty is a valid locked-down state | implementer-default |
| D5 | Permission name and representation | (a) `instance:workspace_create` as a separate `InstancePermission` type in `src/auth/instance.ts`, new `access: "instance"`; (b) add `instance` to `RESOURCES` | (a) | Name fixed by #54; (b) would give it to every workspace owner/admin via `ALL_PERMISSIONS`, make it a token scope and need a `0103` seed migration | implementer-default |
| D6 | Denial status, body and audit | (a) 403 `forbidden` "missing permission instance:workspace_create", audited through the existing pipeline (`workspace_id` null); (b) 404 to hide the route | (a) | Same shape as `authorize()`; route is public knowledge in OpenAPI | implementer-default |
| D7 | Can API tokens create workspaces? | (a) no, keep `userOnly`; (b) instance-scoped tokens | (a) | Unchanged behaviour; tokens are bound to one workspace | implementer-default |
| D8 | Interaction with `NURTUO_INSECURE_NO_AUTH=1` | moot | no change; do not parse the new variables in that mode | `POST /v1/workspaces` is already 405 without the auth hook (`api.ts:166`) | implementer-default |
| D9 | Guard `member.add` against pre-registering an instance-admin email? | (a) yes, only instance admins may create a new account for such an email; (b) no, document the risk | (a) | Without it a workspace admin can create `alice@…` with their own password and inherit the grant; small change, but it adds a 403 case to another route | needs-owner |
| D10 | Gate other workspace-lifecycle routes in phase 1? | (a) no; (b) also gate `DELETE /v1/workspaces/{ws}` / member routes by instance permission | (a) | They are already per-workspace permissions (`workspace:delete` owner only, `member:manage`); not part of #54 | implementer-default |
| D11 | Docs and OpenAPI | (a) description-only OpenAPI change + `auth.md`, `security.md`, `operations.md`; keep the "not partitioned" Known gap | (a) | No schema change needed; the data gap remains until #55 | implementer-default |
| D12 | Workspaces created by ordinary users before the upgrade | (a) leave them, ship audit queries in the release note; (b) migration that demotes non-admin owners; (c) refuse to start until acknowledged | (a) | (b) is destructive and can lock teams out of their own workspaces; (c) blocks upgrades. The residual risk is real until #55 | needs-owner |
| D13 | Expose instance permissions in `GET /v1/auth/me`? | (a) not in phase 1; (b) additive field `instancePermissions` | (a) | No first-party UI creates workspaces; additive later without break | implementer-default |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Before the upgrade, `viewer@corp` created workspace `mine` and is its owner; after the upgrade they still delete any component of `default` with `X-Nurtuo-Workspace: mine`, because rows are global. | high | upgrade, authz | D12 release-note queries; closed for good by #55 |
| 2 | An instance admin creates workspace `team-b` and makes a contractor its owner; the contractor now has every data permission over every row of every workspace. | high | authz (until #55) | documented in `auth.md` Instance admins: "until #55, owning any workspace means full data access"; #55 |
| 3 | An embedder wires OIDC JIT with a lax IdP; an attacker registers `ops@corp` at the IdP, JIT creates the local account and the attacker inherits the instance-admin grant. | medium | identity | `main.ts` does not wire OIDC (auth-rbac-audit gap 5); follow-up: JIT refuses instance-admin emails, same guard as D9 |
| 4 | An admin leaves the company; the env still lists their email; their account remains active and can create workspaces. | medium | operations | disable the user (no API yet, auth-rbac-audit gap 3) and edit the env; DB-backed instance roles (Q6) |
| 5 | An operator sets `NURTUO_ALLOW_WORKSPACE_CREATE=any` to unblock a user and forgets it; the escalation is open again. | medium | operations | startup warning on every start; `security.md` names the risk |
| 6 | Several replicas with different env values give inconsistent answers for the same user. | low | operations | same deployment config for all replicas (as for `NURTUO_AUTH_SECRET`); documented |
| 7 | Denial audit rows have `workspace_id = NULL`, so no workspace owner can see attempted escalations through the API. | low | audit | existing auth-rbac-audit gap 11 (instance-level audit endpoint) |

## Improvement opportunities

- DB-backed instance roles with an `instance:admin` role managing users, instance audit and workspace
  creation (Q6), replacing the env list.
- `GET /v1/auth/me` `instancePermissions` so a future UI can hide a "new workspace" button (D13).

## References

- Parent: [workspace-isolation](2026-11-22-workspace-isolation.md); as built: [auth-rbac-audit](implemented/2026-11-21-auth-rbac-audit.md),
  [control-plane-api-and-data](implemented/2026-11-21-control-plane-api-and-data.md).
- Open Mercato: `tenant-ownership-and-module-acl-authorization`, `org-scope-fail-open-authorization-hardening`.

## Changelog

- 2026-11-24: created as draft.
