<!-- SPDX-License-Identifier: Apache-2.0 -->
# Pre-implement: Gate `POST /v1/workspaces` behind `instance:workspace_create`

**Readiness:** no-go
**Spec:** docs/specs/2026-11-24-workspace-create-gate.md

Date: 2026-11-24. Analysis only. The spec's product decisions were not changed. One factual correction was
applied to the spec before this report: the Contracts row for `NURTUO_BOOTSTRAP_EMAIL` now states that the
draft backward-compatibility contract classifies `NURTUO_BOOTSTRAP_*` as STABLE (the first version said
only "experimental").

Checked against: `AGENTS.md`, `docs/specs/README.md`, `docs/specs/_template.md`, the spec-writing checklist,
the parent `docs/specs/2026-11-22-workspace-isolation.md`, `docs/specs/implemented/2026-11-21-auth-rbac-audit.md`,
the draft `docs/specs/2026-11-22-backward-compatibility-contract.md`, `docs/auth.md`, `docs/security.md`, and
code on `main` at `7953d4f`: `core/control-plane/src/auth/{policy,middleware,endpoints,service,permissions,authz,types,memory-repository,pg-repository,test-util,index}.ts`,
`src/auth/{rbac,policy,authn}.test.ts`, `src/main.ts`, `src/api.ts`, `openapi.yaml`,
`migrations/0100_auth_identity.sql`, `core/runtime/cmd/nurtuo/standalone.go`, `standalone_enroll.go`.
Lessons: `docs/lessons/` has no record for auth, RBAC or the control plane (the two records cover CI label
gates and proto descriptors); none applies.

## Blockers

- **Status is `draft`.** The readiness gate requires `approved` (spec README, AGENTS.md "Never start code
  for sizeable work whose spec is draft"). This is security work and adds runtime config keys, so it is
  sizeable and Ask First.
- **Five `needs-owner` decisions are unresolved:** D1 (instance-admin set via env + bootstrap email, no
  ADR), D2 (default `admins`, escape hatch `any`; and whether the implementation PR needs
  `needs-owner-review`), D3 (bootstrap user = `NURTUO_BOOTSTRAP_EMAIL` whenever set), D9 (`member.add`
  pre-registration guard), D12 (no automatic revocation of owners of workspaces created before the
  upgrade). Each changes who may create workspaces or the scope of #54.

## Findings

- **Critical: readiness gate failed (Status draft, blocking Open Questions Q1 to Q5).** Evidence: spec header
  and Open Questions. Fix: owner answers D1, D2, D3, D9, D12; record answers in the Decisions table; set
  `approved` with a changelog line.
- **Warning: the epic wording "(approved 2026-11-23)" in #66 is not spec approval.** Evidence: parent spec is
  still `Status: draft`, had no INDEX row before this PR. Fix: none in the spec (it already says so under
  Drift); the owner should approve this spec explicitly.
- **Warning: D9 extends #54 beyond its bullet list** (adds a 403 case to `POST /v1/workspaces/{ws}/members`).
  Evidence: issue #54 "Work" lists only the permission, the set and the gate. The spec justifies it (an
  admin of any workspace can create a not-yet-registered admin email with a chosen password via
  `endpoints.ts` `member.add`, which calls `createPasswordUser` for unknown emails). Fix: owner decides;
  if rejected, move the scenario to Gaps with severity high, because the email-based admin set is then
  bypassable by any workspace admin.
- **Warning: env stability conflict between docs.** `auth-rbac-audit` marks `NURTUO_BOOTSTRAP_*` experimental;
  the draft backward-compatibility contract marks control-plane env STABLE. The spec extends the meaning of
  `NURTUO_BOOTSTRAP_EMAIL` additively and now records both classifications. Fix: none blocking; if the
  compat contract is approved first, the new keys must be listed there.
- **Important: behaviour change on a documented REST operation.** `POST /v1/workspaces` returns 403 instead of
  201 for non-admins in the default mode. Path, method and schemas are unchanged and `403` is already in
  `openapi.yaml`, so `oasdiff`-style checks would not flag it. The spec has a Compatibility section with a
  migration path (`NURTUO_ALLOW_WORKSPACE_CREATE=any`, `NURTUO_INSTANCE_ADMINS`) and release-note queries.
  No first-party caller exists (verified: no reference in `sdk/`, `cli/`, `core/web`, `core/mcp`,
  `core/runtime`). Fix: none beyond D2.
- **Important: residual escalation for existing installs (spec gap 1, D12).** Owners of workspaces created
  before the upgrade keep full data access until #55. Acceptance criteria correctly say "closed for the
  default configuration", i.e. fresh state. Fix: owner confirms D12; release note must carry the queries.
- **Important: `setup()` becomes fail-closed for workspace creation.** With `AuthServiceOptions.instance`
  omitted, no one can create workspaces. Verified that only `rbac.test.ts:183` creates a workspace through
  the API (`authn.test.ts`, `pg-auth.test.ts` and `policy.test.ts` use `authRepo.createWorkspace` or no
  workspace creation), so one existing test changes. The spec names it. Fix: none.
- **Important: `policy.test.ts` `policy table is well-formed` must learn `access: "instance"`.** Today it asserts
  `p.permission === undefined` for non-`permission` policies (`policy.test.ts:48-49`), which still passes, but
  nothing would check `instancePermission`. The spec's step 4 covers it. Fix: none.
- **Important: `src/main-config.test.ts` spawns `dist/main.js`.** `main.ts` uses top-level await and starts an
  HTTP server, so the test relies on the config parse running before the DB and listen calls (spec step 6
  says so) and on `PORT=0`/timeout to avoid hanging. It is picked up by the package's
  `node --test "dist/**/*.test.js"`. Fix: keep the parse first in `main.ts`; the test must kill the child.
- **Spec matches the code** on every named surface checked: `policy.ts:81` (`authed(..., { userOnly: true })`),
  `RoutePolicy.access` union (`policy.ts:14`), `middleware.ts:143` and `:155`, `endpoints.ts:135`/`:162`/`:172`,
  `service.ts:76` (`bootstrap` does not add membership to an existing `default`), `permissions.ts:6`/`:32`
  (`owner = ALL_PERMISSIONS`, which is why `instance` must stay out of `RESOURCES`), `authz.ts:11`, `main.ts:41`,
  `api.ts:166` (405 without auth hook), `standalone.go` `controlPlaneEnv` (always passes
  `NURTUO_BOOTSTRAP_EMAIL=admin@nurtuo.local`), `users_email_lower` / `users_email_key` in `0100`. No
  mis-description found.
- **Repo rules:** new TS file in `core/` with AGPL SPDX header (spec says so); no import from Apache-2.0 code;
  no new dependency; no proto, flow, bundle, CLI or MCP change; tests in the same PR; no changeset needed
  (`core/control-plane` is AGPL, not a consumed package). Runtime config keys are a public contract (Ask
  First before implementation), covered by approving this spec.
- **Scope cohesion:** one shippable slice (#54). Phase 2 (#55) and #57 (Windows DACL) are explicit
  non-goals; #57 has no dependency on this spec.

## Risks

- **High:** pre-upgrade self-created workspaces (gap 1) and instance-admin-created workspaces with non-admin
  owners (gap 2) keep global data access until #55. Mitigated by documentation only; real fix is #55.
- **Medium:** email-based admin set with OIDC JIT in embedders (gap 3); stale env entries (gap 4); forgotten
  `any` (gap 5). Mitigations listed in the spec.
- **Low:** replica config drift (gap 6), null-workspace audit rows (gap 7).

## During implementation

Not applicable (verdict is no-go). Once approved, re-run this check; the items under "Important" are
already part of the step plan.

## Next action

- Owner answers D1, D2, D3, D9, D12 on PR or issue #54; the spec author records them, removes resolved Open
  Questions and sets `approved` with a changelog line. Then re-run pre-implement and implement under
  `Closes #54`.

```
Readiness: no-go
Report: docs/specs/analysis/pre-implement-workspace-create-gate.md
Spec: docs/specs/2026-11-24-workspace-create-gate.md
```
