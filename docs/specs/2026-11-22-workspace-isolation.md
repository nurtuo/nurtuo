<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Workspace isolation of control plane data

Status: draft. Date: 2026-11-22.

> Phase 1 (workspace creation gate, #54) is specified in `docs/specs/2026-11-24-workspace-create-gate.md` (2026-11-24), which Extends this spec.
>
> The DB-backed `instance_roles` model from gap 4 is tracked in #77 (not in #55); phase 1 uses a temporary env-var admin list (decision D1 of the phase-1 spec).

## TLDR

Give every component, revision, runtime, desired state and deployment an owning workspace, make every
repository call take that workspace, and make the auth hook pass the resolved workspace to the data
routes, so a principal can only ever see or change rows of the workspace it was authorized in.
Missing or mismatched workspace means "not found", never "all rows". Existing rows move to the
workspace `default` in one migration.

## Status and links

| | |
|---|---|
| Pull requests | none yet |
| ADRs | a new ADR for "workspace is the tenant boundary; runtimes belong to one workspace" |
| Issues | epic https://github.com/nurtuo/nurtuo/issues/66; phase 1 https://github.com/nurtuo/nurtuo/issues/54; phase 2 https://github.com/nurtuo/nurtuo/issues/55 |
| Code | `core/control-plane/migrations/`, `core/control-plane/src/{repository,memory-repository,pg-repository,api,gateway}.ts`, `core/control-plane/src/auth/{middleware,policy,endpoints}.ts` |
| User docs | [auth.md](../auth.md#known-gaps), [security.md](../security.md) |
| Evidence | [M1 review guide](../review/PR-STACK.md): "roles and tokens are not yet partitioned per workspace"; `auth.md` Known gaps; as-built specs `control-plane-api-and-data` (gap 1) and `auth-rbac-audit` (gap 1) |

## Problem and goals

Today authorization is per workspace but data is global:

- `components`, `revisions`, `runtimes`, `desired_state`, `deployments` and `outbox` have no
  `workspace_id` (migrations `0001` to `0003`).
- Data routes use `policy.workspace: "context"`; the middleware resolves a workspace, checks the
  permission there, then calls `api.ts` `route(repo, req)`, which ignores it.
- Any session user may `POST /v1/workspaces` and becomes `owner` of the new workspace, so any user
  can obtain every data permission over every row (critical, `auth-rbac-audit` gap 1).
- Roles are global built-ins (`roles.name` primary key, no workspace). Tokens are already bound to one
  workspace (`api_tokens.workspace_id`), but that binding only limits which workspace they may name,
  not which rows they reach.

Goals:

- A principal authorized in workspace W reads and writes only rows with `workspace_id = W`.
- Fail closed: a repository method cannot be called without a workspace; a route without a resolved
  workspace cannot reach data.
- Runtimes belong to exactly one workspace; a runtime's certificate identity can only ever see its
  own workspace's desired state.
- Component names are unique per workspace, not globally.
- Existing single-tenant installs keep working after the upgrade with no manual steps.
- Workspace creation is gated by an instance-level permission.

Non-goals: custom roles per workspace (later spec), cross-workspace sharing of components,
Postgres row-level security as the only control (it may be added as defence in depth, see below),
separate databases per tenant.

## Design

### Data model (migration `0200_workspace_isolation.sql`)

Numbered `0200` to stay clear of the `0100` auth range.

```sql
ALTER TABLE components    ADD COLUMN workspace_id text REFERENCES workspaces(id) ON DELETE RESTRICT;
ALTER TABLE runtimes      ADD COLUMN workspace_id text REFERENCES workspaces(id) ON DELETE RESTRICT;
ALTER TABLE deployments   ADD COLUMN workspace_id text REFERENCES workspaces(id) ON DELETE RESTRICT;
ALTER TABLE desired_state ADD COLUMN workspace_id text REFERENCES workspaces(id) ON DELETE RESTRICT;
ALTER TABLE outbox        ADD COLUMN workspace_id text;   -- informational, no FK

-- backfill: everything that exists belongs to the bootstrap workspace
INSERT INTO workspaces(id, slug, name) SELECT gen_random_uuid()::text, 'default', 'default'
  WHERE NOT EXISTS (SELECT 1 FROM workspaces WHERE slug = 'default')
    AND (EXISTS (SELECT 1 FROM components) OR EXISTS (SELECT 1 FROM runtimes));
UPDATE components    SET workspace_id = (SELECT id FROM workspaces WHERE slug = 'default') WHERE workspace_id IS NULL;
-- same for runtimes, deployments, desired_state, outbox

ALTER TABLE components ALTER COLUMN workspace_id SET NOT NULL;   -- and the other three FK tables
ALTER TABLE components DROP CONSTRAINT components_name_key;
CREATE UNIQUE INDEX components_ws_name ON components (workspace_id, name);
CREATE INDEX runtimes_ws_env ON runtimes (workspace_id, environment);
CREATE INDEX deployments_ws_runtime ON deployments (workspace_id, runtime_id, created_at DESC);
```

- `revisions` inherit the workspace through `component_id`; queries always join or filter by
  `components.workspace_id`.
- A deployment must reference a component and a runtime of the same workspace: enforced in the
  `deploy` transaction (`SELECT ... WHERE id = $1 AND workspace_id = $2` for both) and by a composite
  foreign key `(component_id, workspace_id)` on `components(id, workspace_id)` (and the same for
  runtimes), which needs `UNIQUE (id, workspace_id)` on both parent tables.
- `ON DELETE RESTRICT` on the workspace FKs: deleting a workspace that still has components or
  runtimes returns 409 `workspace_not_empty` instead of silently cascading deployments away. The
  `workspace.delete` endpoint checks this first.
- `runtimes.id` stays globally unique (it is the certificate identity, `spiffe://nurtuo/runtime/<id>`).
  Registering an id that exists in another workspace is 409 `conflict` and does not disclose which
  workspace owns it.

If a workspace with slug `default` already exists (the `bootstrap` default), the backfill uses it;
the migration cannot tell whether that was the intended owner, so the release note says where the
rows went. `gen_random_uuid()` needs PostgreSQL 13 or later. The migration is a single transaction
(as `migrate()` does for every file); on failure nothing changes.

### Repository contract

Every `Repository` method takes a first argument `ws: string` (the workspace id), for example
`listComponents(ws)`, `getComponent(ws, id)`, `deploy(ws, input)`. Rules:

1. There is no overload without `ws`. TypeScript makes forgetting it a compile error.
2. Lookups by id filter by `workspace_id = ws`; a row of another workspace is reported exactly like a
   missing row (`undefined`, HTTP 404).
3. List methods filter by `ws` in SQL, never in application code after a global read.
4. `MemoryRepository` stores `workspaceId` on each row and applies the same filters, so handler tests
   cover the semantics without Postgres.
5. The gateway uses a separate narrow interface `GatewayRepository` keyed by runtime id
   (`touchRuntime`, `getDesiredState`, `getRevisionById` restricted to revisions referenced by that
   runtime's desired state, `listDeployments` for that runtime), because a runtime connection has no
   user principal. `getRevisionById(id)` without a workspace is removed from the public `Repository`.

### Request path

- `api.ts` `route(repo, req, ctx)` receives `ctx.workspaceId`. `createAuthHook` passes the resolved
  workspace as `next(req, { workspaceId })`.
- `auth: "disabled"` (tests and `NURTUO_INSECURE_NO_AUTH=1`) uses the workspace from
  `X-Nurtuo-Workspace`, else `default`; it never means "all workspaces".
- `route` refuses with 500 `internal` (and a log line) if called without `ctx.workspaceId`: a wiring
  bug must not degrade to global access.
- Data routes keep `workspace: "context"`. A user with several memberships must send
  `X-Nurtuo-Workspace` (400 `workspace_required` as today).
- Optional later: path-scoped aliases `/v1/workspaces/{ws}/components/...` with identical policies,
  for clients that prefer explicit URLs. Out of scope for the first PR.

### Workspace creation

- New instance-level permission `instance:workspace_create`, held by members of an instance-admin
  list (`NURTUO_INSTANCE_ADMINS`, comma-separated emails, plus the bootstrap user). `POST /v1/workspaces`
  requires it. Config `NURTUO_ALLOW_WORKSPACE_CREATE=any` restores today's behaviour for
  self-hosted single-team installs, defaulting to `admins`.
- This closes the escalation even before data partitioning ships, so it is phase 1.

### Roles and tokens

- Roles stay global built-ins in this spec; the permission matrix is unchanged. Custom roles per
  workspace are a later spec (add `roles.workspace_id NULL` = built-in).
- API tokens already carry `workspace_id`; with partitioned data a token reaches only its workspace's
  rows. Tokens with the `runtime` role that runtimes may use over REST (`POST /v1/runtimes`,
  `PUT /v1/deployments/{id}/status`) can only touch runtimes of that workspace.
- The gateway binds a certificate to one runtime row, so to one workspace. A desired state push only
  contains revisions of that workspace (enforced by the composite FK).

### Fail-closed rules (summary)

| Situation | Result |
|---|---|
| Repository called without workspace | compile error; at runtime 500 |
| Row exists in another workspace | 404 `not_found` (same body as missing) |
| Deploy component of W1 to runtime of W2 | 404 (`runtime not found`) |
| Runtime id registered in another workspace | 409 `conflict`, no workspace disclosed |
| Delete workspace with components or runtimes | 409 `workspace_not_empty` |
| Data route with `auth: "disabled"` and no header | workspace `default`; 404 if it does not exist |
| Session user, several memberships, no header | 400 `workspace_required` (unchanged) |
| `POST /v1/workspaces` without `instance:workspace_create` | 403 `forbidden` |

### Defence in depth (optional, phase 3)

Postgres row-level security on the five tables with `current_setting('nurtuo.workspace_id')`, set by
`PgRepository` per transaction with `SET LOCAL`. Not a replacement for the repository filters: the
gateway and migrations would need a bypass role.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `Component`, `Runtime`, `Deployment`, `DesiredState` JSON | new field `workspaceId` | experimental |
| `POST /v1/components` | `name` unique per workspace (409 only within the workspace) | experimental |
| `POST /v1/runtimes` | 409 when the id exists in another workspace | experimental |
| `DELETE /v1/workspaces/{ws}` | new 409 `workspace_not_empty` | experimental |
| `POST /v1/workspaces` | requires `instance:workspace_create` (403) unless `NURTUO_ALLOW_WORKSPACE_CREATE=any` | experimental |
| Environment | new `NURTUO_INSTANCE_ADMINS`, `NURTUO_ALLOW_WORKSPACE_CREATE` (`admins` default, `any`) | experimental |
| `Repository` interface | every method takes `ws` first; `GatewayRepository` split out | internal |
| SQL | migration `0200_workspace_isolation.sql` | internal |

## Compatibility

- Single-workspace installs: the migration assigns all rows to `default`; clients that never sent
  `X-Nurtuo-Workspace` keep working because users with one membership and tokens resolve implicitly.
- Multi-workspace installs that relied on seeing all components from any workspace: after upgrade
  each workspace sees only what was moved into `default`. Operators move rows with a documented SQL
  snippet (`UPDATE components SET workspace_id = ... WHERE name IN (...)` together with their
  deployments and runtimes) before letting users in; the release notes must say so.
- Component names that collide across workspaces become possible; clients that use the name as a
  global key (`ControlPlaneFlowStore` uses names as flow ids) must send the workspace header.
- `POST /v1/workspaces` by ordinary users stops working by default; set
  `NURTUO_ALLOW_WORKSPACE_CREATE=any` to keep it.
- Downgrade: not supported after `0200` (forward-only migrations); take a database backup first.

## Security

The point of this spec. Additional rules: 404 instead of 403 for rows of another workspace, so ids
cannot be probed; audit rows keep `workspace_id` of the resolved workspace (already the case);
`beforePath` snapshot requests in the auth hook must carry the same workspace context, otherwise the
before-hash is taken from the wrong row (or none).

## Testing

New tests (names are the acceptance criteria):

| Test | File | What it proves |
|---|---|---|
| `isolation: components of B are invisible to A (list, get, revisions, patch, delete)` | `src/auth/isolation.test.ts` | per-route filtering, 404 not 403 |
| `isolation: deploy across workspaces is 404 and writes no desired state or outbox row` | same | deploy transaction checks both sides |
| `isolation: same component name in two workspaces` | same | per-workspace uniqueness |
| `isolation: runtime id taken in another workspace is 409 without disclosure` | same | |
| `isolation: workspace creation needs instance:workspace_create` | same | phase 1 escalation fix |
| `isolation: viewer who creates a workspace cannot touch the original workspace` | same | regression for `auth-rbac-audit` gap 1 |
| `isolation: auth disabled uses default workspace, never all rows` | `src/api.test.ts` | |
| `isolation: every Repository method requires ws` | `src/repository.contract.test.ts` | run against `MemoryRepository` and, with `DATABASE_URL`, `PgRepository` |
| `migration 0200: backfills default, enforces NOT NULL and composite FKs` | `src/pg-isolation.test.ts` | `DATABASE_URL`-gated; must run in CI with a Postgres service |
| `gateway: desired state only contains revisions of the runtime's workspace` | `src/gateway.test.ts` | |
| `delete workspace with components is 409 workspace_not_empty` | `src/auth/rbac.test.ts` | |

The `role x route matrix` test in `rbac.test.ts` is extended to run every data route with two
workspaces populated and assert that response bodies contain only rows of the caller's workspace.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A new data route or repository method is added with a global query (for example a future `listAllDeployments` for a dashboard). | high | maintenance | contract test that calls every `Repository` method with a foreign workspace; review checklist line in `docs/specs/README.md` once approved |
| 2 | The backfill puts rows of a deployment that used several workspaces informally into `default`; users of other workspaces lose visibility on upgrade and redeploy duplicates. | medium | upgrade | release note + SQL snippet; a dry-run mode `nurtuo-control-plane migrate --plan` printing row counts |
| 3 | Composite foreign keys add `UNIQUE (id, workspace_id)` indexes on large tables; on an install with millions of deployments the migration locks `deployments` for minutes. | low | upgrade | M1 installs are small; document `CREATE INDEX CONCURRENTLY` outside the transaction if needed |
| 4 | Instance-admin list via environment variable drifts from reality (admin leaves, env not updated). | medium | operations | phase 2: `instance_roles` table managed through the API |

## Implementation phases

1. Gate `POST /v1/workspaces` behind `instance:workspace_create` (closes the critical escalation).
2. Migration `0200`, repository signatures, request context, tests above, docs (`auth.md` Known gaps
   removed, `security.md` table updated).
3. Optional row-level security and path-scoped route aliases.

## References

- Open Mercato: `tenant-ownership-and-module-acl-authorization`, `org-scope-fail-open-authorization-hardening`,
  `crud-fail-open-hardening`.
- As-built specs `control-plane-api-and-data`, `auth-rbac-audit`.

## Changelog

- 2026-11-22: created (draft).
