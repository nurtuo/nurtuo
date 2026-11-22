<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Authentication, RBAC and audit log (control plane)

Status: implemented. Date: 2026-11-21 (written as built on 2026-11-22).

## TLDR

Every control plane REST request goes through one hook (`createAuthHook`, `core/control-plane/src/auth/middleware.ts`):
match a route policy (no policy means denied), authenticate (session cookie, session JWT or API
token), check CSRF for cookies, resolve the workspace, check a `resource:action` permission of the
caller's role there, run the handler, and append every mutation to an append-only audit log. The one
thing to know: authorization is per workspace, but the **data it guards is not** (see
`control-plane-api-and-data` and draft `workspace-isolation`), so in M1 a control plane is safe only
as a single-tenant install.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/27 (squash commit `4541089`), https://github.com/nurtuo/nurtuo/pull/39 (squash commit `e22ffab`, flaky API-token test fix) |
| ADRs | none |
| Issues | none |
| Code | `core/control-plane/src/auth/`, migrations `core/control-plane/migrations/0100_auth_identity.sql` to `0103_auth_seed_roles.sql`, wiring in `core/control-plane/src/main.ts` |
| User docs | [auth.md](../../auth.md), [security.md](../../security.md) |
| Related specs | `control-plane-api-and-data`; drafts `workspace-isolation`, `auth-rate-limiting-and-lockout`, `enterprise-identity-saml-scim` |

## Problem and goals

The control plane can deploy code to runtimes that reach production systems. It needs to know who is
calling, what they may do, and keep a record of every change, without a paid edition
("Everything free", README).

- Goals: password sessions for humans, scoped API tokens for machines and CI, OIDC SSO, built-in roles
  with an escalation-proof grant rule, CSRF protection for browser sessions, fail-closed routing,
  tamper-resistant audit.
- Non-goals (M1): SAML and SCIM (routes return 501), MFA, password reset, self-service sign-up, email
  verification, custom roles, per-workspace data partitioning.

## Design (as built)

### Files

| File | Responsibility |
|---|---|
| `permissions.ts` | `RESOURCES`, `ACTIONS`, `ALL_PERMISSIONS`, `ROLES`, `ROLE_PERMISSIONS`, `canGrant`. Source of truth; `0103_auth_seed_roles.sql` mirrors it. |
| `policy.ts` | `POLICIES` (one `RoutePolicy` per method + path), `matchPolicy`, `authorize(permission, resource)`, `MUTATING`. |
| `authz.ts` | `permissionsIn(principal, workspaceId)`: role permissions there, intersected with token scopes for tokens. |
| `crypto.ts` | scrypt passwords, HS256 JWT, API token format, `safeEqual`. Only `node:crypto`. |
| `service.ts` | `AuthService`: login, sessions, `authenticate`, `mintToken`, `bootstrap`, OIDC state signing; `RateLimited` (429). |
| `middleware.ts` | `createAuthHook`: the request pipeline and the audit writer. |
| `endpoints.ts` | Handlers for auth-owned routes (`auth.*`, `role.list`, `workspace.*`, `member.*`, `token.*`, `audit.query`). |
| `oidc.ts` | `OidcClient` (authorization code + PKCE S256, RS256 ID token checked against JWKS). |
| `throttle.ts` | In-process `RateLimiter` (fixed window) and `FailureTracker`. |
| `enterprise-stubs.ts`, `stub-routes.ts` | `SamlProvider` and `ScimBackend` interfaces; every SAML/SCIM route answers 501. |
| `repository.ts`, `memory-repository.ts`, `pg-repository.ts` | `AuthRepository` and its two implementations. |
| `test-util.ts` | Test harness (`setup()`), not imported by production code. |

### Request pipeline (`createAuthHook(...).handle`)

1. `matchPolicy(method, path)`. Public routes skip authentication and ignore any credentials.
2. `service.authenticate(headers)`: `Authorization: Bearer nrt_...` (API token), `Authorization: Bearer <jwt>`
   (session JWT) or cookie `__Host-nurtuo_session`. Credentials that are present but invalid are 401
   `unauthenticated` with `WWW-Authenticate: Bearer realm="nurtuo"`.
3. No policy matched: 401 if anonymous, else 403 `no_policy` (fail closed).
4. `userOnly` routes (`GET`/`POST /v1/workspaces`) refuse API tokens (403).
5. Cookie principal + mutating method: header `X-Nurtuo-Csrf` must equal the JWT's `csrf` claim
   (`safeEqual`), else 403 `csrf`.
6. Workspace resolution: `path` policies take `:ws` (id or slug) and answer 404 for non-members (and
   for tokens of another workspace) so existence is not disclosed. `context` policies take
   `X-Nurtuo-Workspace`, else the token's workspace, else the user's only membership; several
   memberships and no header is 400 `workspace_required`.
7. `authorize(policy.permission, {workspaceId})`: 403 `forbidden` when `permissionsIn` lacks it.
8. Handler: `endpoints[policy.action]` or, for data routes, `api.ts` via `next`. For `PATCH`/`PUT`/`DELETE`
   with `beforePath`, the hook first calls `next` with an internal `GET` to snapshot the before-state.
9. Audit: every mutating request by an authenticated caller, including 4xx results and denials at
   steps 6 and 7; for public routes only `auth.login` and `auth.oidc_callback`. If the append throws,
   the response is 500 `audit_unavailable` (the mutation has already happened).
10. Every response gets `x-request-id` (the caller's `X-Request-Id` if it matches
    `^[A-Za-z0-9._-]{8,64}$`, else a UUID) and `cache-control: no-store`.

### Roles and permissions

18 permissions over 7 resources (`component`, `runtime`, `deployment`, `workspace`, `member`, `token`,
`audit`). Built-in roles: `owner` (all), `admin` (all but `workspace:delete`), `editor` (view +
`component:write/delete` + own tokens), `viewer` (view), `deployer` (view + `deployment:create` + own
tokens), `runtime` (machine role: `component:read`, `runtime:read/register`, `deployment:read/report`).
`canGrant(held, role)` allows granting, changing or removing a holder of a role only if the caller holds
every permission of that role. The repository refuses to demote or remove the last owner
(`setMember`/`removeMember` lock owner rows `FOR UPDATE`, result `last_owner`, HTTP 409).

### Credentials

- **Passwords**: scrypt (`N=16384, r=8, p=1`, 16-byte salt, 32-byte key, NFKC), stored as
  `scrypt$N$r$p$salt$hash`; `verifyPassword` rejects out-of-range stored parameters (`N` above `2^17`)
  and compares with `timingSafeEqual`. Length 12 to 256. Unknown accounts still run one scrypt against
  a dummy hash, and the error is always 401 `invalid_credentials`.
- **Sessions**: `sessions` row + HS256 JWT `{iss: nurtuo-control-plane, aud: nurtuo-api, sub, sid, csrf, iat, exp}`,
  TTL 8 h, cookie `__Host-nurtuo_session; Path=/; HttpOnly; Secure; SameSite=Strict`. `verifyJwt` pins
  header `alg: HS256, typ: JWT`, checks signature length and value in constant time, `iss`, `aud`, `exp`,
  `nbf`. Every request re-reads the session (revoked, expired, user mismatch) and the user (`active`).
  `POST /v1/auth/logout` revokes the session.
- **API tokens**: `nrt_<12 hex>_<43 base64url>` (`generateApiToken`, `parseApiToken`). Only
  `sha256(secret)` is stored (`api_tokens.hash`), looked up by `prefix`, compared with `safeEqual`; an
  unknown prefix is compared against a dummy hash. A token is bound to one workspace, carries scopes,
  default TTL 90 days, max 365 days, and a token-minted token never outlives its parent. Scopes must
  be held by the minter (403 `scope_exceeds_permissions`). The creator must still be an active member
  of the token's workspace on every request; `removeMember` also revokes their tokens there.
  `last_used_at` is written at most once a minute.
- **Throttling**: per-IP fixed window on `auth.login` and `auth.oidc_callback` (30/min,
  `loginsPerMinutePerIp`), per-account lockout after 5 failures for 15 min (`users.failed_logins`,
  `locked_until`, atomic `UPDATE`), unknown emails tracked in memory by `FailureTracker`. 429 with
  `Retry-After` (`rate_limited` or `account_locked`). Details and gaps: draft `auth-rate-limiting-and-lockout`.
- **OIDC**: `OidcClient.begin` creates `state`, `nonce` and a PKCE verifier, kept in a 10-minute
  HMAC-signed cookie `__Host-nurtuo_oidc` (`SameSite=Lax`). `complete` checks state, exchanges the code,
  verifies the ID token (RS256 only, `kid` lookup in JWKS, `iss`, `aud`, `azp` when several audiences,
  `exp` with 60 s skew, `iat` not more than 300 s ahead, `nonce`). Account mapping: `(oidc:<issuer>, sub)`
  in `external_identities`; else opt-in `linkByVerifiedEmail`; else opt-in JIT into one workspace
  with a role that `createAuthHook` refuses to be `owner` or `admin`.

### Audit log

`audit_log` (`0102_audit_log.sql`): `seq bigserial`, `id`, `ts`, `workspace_id` (no foreign key, so rows
survive deletion), `actor_type` (`user`, `token`, `anonymous`), `actor_id` (token id for tokens, with
`metadata.userId`), `action` and `resource` from the policy, `resource_id`, `before_hash` and
`after_hash` (`hashJson`: `sha256:` of sorted-key JSON, never the content), `ip`, `request_id`,
`status`, `metadata`. Triggers `audit_log_no_update_delete` and `audit_log_no_truncate` raise on
UPDATE, DELETE and TRUNCATE. `GET /v1/workspaces/{ws}/audit` filters by `actor`, `action`, `resource`,
`resourceId`, `status`, `from`, `to`, with `limit` 1 to 200 and a `seq` cursor, newest first. Login
audit rows have `workspace_id = NULL` and are therefore not visible through the API.

### Wiring (`src/main.ts`)

`NURTUO_AUTH_SECRET` (at least 32 bytes; required with `DATABASE_URL`, else a random per-process
secret), `NURTUO_BOOTSTRAP_EMAIL` + `NURTUO_BOOTSTRAP_PASSWORD` (idempotent `bootstrap`: user + workspace
`default` as owner), `NURTUO_TRUST_PROXY=1` (use the first `X-Forwarded-For` entry as client IP),
`NURTUO_INSECURE_NO_AUTH=1` (dev only, every route open, logs a warning). `main.ts` does not pass an
`oidc` option to `createAuthHook`, so OIDC is available to embedders of the package but not in the
shipped entry point.

### PR #39

`api tokens: bearer auth, hashed at rest, expiry, revocation, last-used` checked that the secret part
of a token never appears in the stored record by taking `token.split("_")[2]`. The base64url secret can
contain `_`, so the test sometimes checked only a short fragment that could match elsewhere. It now
takes everything after the prefix. No production code changed.

## Contracts

| Contract | Stability | Notes |
|---|---|---|
| Routes `/v1/auth/{login,logout,me}`, `/v1/auth/oidc/{login,callback}`, `/v1/roles`, `/v1/workspaces[/{ws}[/members[/{userId}]|/tokens[/{tokenId}]|/audit]]` | experimental | `openapi.yaml`; consumed by `sdk/ts/client`, `cli`, `core/web` |
| `/v1/auth/saml/{login,acs,metadata}`, `/v1/scim/v2/Users[/{id}]`, `/v1/scim/v2/Groups` | experimental, 501 | see draft `enterprise-identity-saml-scim` |
| Headers: `Authorization: Bearer`, cookie `__Host-nurtuo_session`, `X-Nurtuo-Csrf`, `X-Nurtuo-Workspace`, `X-Request-Id`; responses `x-request-id`, `cache-control: no-store`, `Retry-After`, `WWW-Authenticate` | experimental | |
| API token format `nrt_<12 hex>_<43 base64url>` | stable intent | secret scanners and clients depend on it; changing it needs a compatibility note |
| Permission ids (`resource:action`) and role names | experimental | used as token scopes, so renaming one breaks existing tokens |
| Error codes: `unauthenticated`, `forbidden`, `no_policy`, `csrf`, `workspace_required`, `invalid_credentials`, `rate_limited`, `account_locked`, `role_escalation`, `last_owner`, `scope_exceeds_permissions`, `sso_not_provisioned`, `oidc_failed`, `audit_unavailable`, `not_implemented` | experimental | |
| Environment: `NURTUO_AUTH_SECRET`, `NURTUO_BOOTSTRAP_EMAIL`, `NURTUO_BOOTSTRAP_PASSWORD`, `NURTUO_TRUST_PROXY`, `NURTUO_INSECURE_NO_AUTH` | experimental | |
| SQL: `users`, `external_identities`, `workspaces`, `roles`, `permissions`, `role_permissions`, `memberships`, `api_tokens`, `sessions`, `audit_log` | internal | |
| `AuthRepository`, `OidcProvider`, `SamlProvider`, `ScimBackend` interfaces | internal | exported from `src/auth/index.ts` |

## Security

Covered above. Summary of the defaults: auth on unless `NURTUO_INSECURE_NO_AUTH=1`; unknown routes
denied; secrets compared in constant time (a test greps auth code for `===` on secrets); no plaintext
token or password is stored, logged or audited; JIT can never grant `owner` or `admin`; workspace
existence is not disclosed to non-members.

## Testing

All tests run with `pnpm turbo run typecheck test --filter=@nurtuo/control-plane` (Node test runner on
`dist/**/*.test.js`). They use `setup()` from `src/auth/test-util.ts` (memory repositories, scrypt `N=1024`,
movable clock) unless stated.

| Test | What it covers | Notes |
|---|---|---|
| `src/auth/crypto.test.ts`: `scrypt: verify, wrong password, salting, tampered params`, `jwt: HS256 only, signature, expiry, iss/aud`, `api token format and hashing`, `safeEqual`, `secrets are never compared with ===, ==, or Buffer.equals in auth code`, `permissions: matrix invariants`, `migration 0103 seed matches permissions.ts (no drift)` | primitives, matrix, seed drift | |
| `src/auth/authn.test.ts`: `auth disabled is explicit and keeps legacy behaviour`, `login: success sets a hardened cookie; failures are generic`, `lockout after repeated failures (existing and unknown accounts), then recovery`, `successful login resets the failure counter`, `per-IP rate limit on login`, `sessions: expiry, revocation, disabled users, tampered or foreign JWTs`, `api tokens: bearer auth, hashed at rest, expiry, revocation, last-used`, `removing a member revokes their tokens in the workspace`, `audit: every mutating request is logged, reads are not, secrets never appear`, `audit endpoint: filters and cursor pagination`, `security headers on responses`, `SAML and SCIM are documented stubs returning 501`, `trustProxy only honoured when enabled`, `audit failure fails the request closed` | authentication, throttling, audit | fixed in #39 |
| `src/auth/rbac.test.ts`: `role x route matrix: every permission-gated route, every role`, `session cookie principals follow the same matrix (spot check) and need CSRF to mutate`, `session user with several workspaces must pick one; non-member header is 403`, `escalation: nobody can grant, change or remove a role above their own permissions`, `last owner cannot be demoted or removed; a second owner can`, `token scope widening is impossible`, `token powers track the creator's current membership (demotion shrinks, removal kills)`, `workspace creation is for session users only; creator owns it`, `cross-workspace access is denied for users and tokens` | authorization | the cross-workspace test checks access to workspace-scoped routes and the header, not that data rows are partitioned |
| `src/auth/policy.test.ts`: `openapi.yaml is parseable and non-trivial`, `every openapi operation has an authz policy, and every policy is documented`, `policy table is well-formed`, `openapi declares security, 401 and 403 for every operation`, `runtime: every non-public documented route rejects anonymous callers with 401 and unknown routes are denied`, `runtime: a route without a policy is 403 even for an owner (fail closed)` | fail-closed routing, OpenAPI sync | |
| `src/auth/oidc.test.ts`: `oidc: authorization code + PKCE, JIT provisioning, session issued`, `oidc: no JIT and no link -> sso_not_provisioned; link by verified email is opt-in`, `oidc: attack cases are rejected`, `oidc: authorization code cannot be replayed and PKCE verifier is bound to the browser`, `oidc: not configured -> 404; issuer must be https; JIT cannot grant owner/admin` | OIDC with a fake IdP | no real IdP |
| `src/auth/pg-auth.test.ts`: `postgres auth: migrations, seed matches code, constraints, audit immutability, handler smoke` | migrations `0100`-`0103`, constraints, last-owner guard, lockout counter, append-only triggers, handler on Postgres | **skipped unless `DATABASE_URL` is set** (drops the `public` schema; example in the file header uses `postgres:16-alpine`); never runs in CI |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A `viewer` of workspace `default` calls `POST /v1/workspaces {slug: "mine"}` (allowed for any session user), becomes `owner` of `mine`, and with `X-Nurtuo-Workspace: mine` deletes or deploys any component of `default`, because data routes resolve the workspace only for the permission check (`policy.workspace: "context"`) and the rows are global. | critical | authz, data model | draft `workspace-isolation` (partition data; until then gate workspace creation behind an instance-level permission) |
| 2 | With `NURTUO_TRUST_PROXY=1`, `clientIp` takes the **first** `X-Forwarded-For` entry, which the client controls when the proxy appends. An attacker sends a random `X-Forwarded-For` per request: the per-IP login limit never triggers and audit rows carry a forged IP. | high | throttling, audit | take the right-most untrusted hop (configured trusted proxy count or CIDRs); draft `auth-rate-limiting-and-lockout` |
| 3 | No API to disable a user, revoke all sessions of a user or change a password: `setUserStatus` and `revokeSessionsForUser` exist in `AuthRepository` but no endpoint calls them. After a laptop with a live session is stolen, an operator must run SQL against `users`/`sessions`; the session stays valid for up to 8 h otherwise. | high | incident response | add `POST /v1/users/{id}/disable`, `DELETE /v1/auth/sessions` (all of mine) and a password change endpoint that revokes other sessions; Open Mercato `bearer-logout-session-revocation` |
| 4 | `pg-auth.test.ts` is skipped in CI, so the SQL of `PgAuthRepository` (lockout `UPDATE`, owner locking, audit query) is only tested on a developer machine with `DATABASE_URL`. | high | testing | Postgres service in CI; draft `quality-gates` |
| 5 | OIDC is implemented and tested but `main.ts` never constructs `OidcClient` or passes `oidc` to `createAuthHook`; in the shipped binary `GET /v1/auth/oidc/login` answers 404 `OIDC is not configured`, while [security.md](../../security.md) lists "optional OIDC". | medium | wiring | add `NURTUO_OIDC_ISSUER`, `NURTUO_OIDC_CLIENT_ID`, `NURTUO_OIDC_CLIENT_SECRET`, `NURTUO_OIDC_REDIRECT_URI`, JIT settings; draft `enterprise-identity-saml-scim` phase 0 |
| 6 | The audit row is written after the mutation and outside its transaction. If Postgres fails between the two, the client gets 500 `audit_unavailable` although the deployment was created; the client retries and a second deployment (generation + 1) is created, and the first has no audit row. | medium | audit | write audit rows in the same transaction (repository-level unit of work) or via an outbox; idempotency keys on `POST /v1/deployments` |
| 7 | Per-account lockout (5 failures, 15 min) is keyed on the email only. Anyone who knows the owner's email can keep that account locked forever with 5 requests every 15 minutes, from any IP. | medium | availability | draft `auth-rate-limiting-and-lockout` (lockout keyed on account + IP, captcha-free backoff, admin unlock) |
| 8 | `RateLimiter` and `FailureTracker` are in-process. With 3 replicas the effective login limit is 90/min per IP, and restarting the process clears unknown-email lockouts. | medium | throttling | shared store (Postgres table); draft `auth-rate-limiting-and-lockout` |
| 9 | `POST /v1/workspaces/{ws}/members` creates a new local account with a password chosen by the admin; there is no invite, no forced change on first login and no reset flow. The admin knows every new user's password. | medium | identity lifecycle | invite tokens with a single-use set-password link; or OIDC-only installs |
| 10 | One HS256 secret signs session JWTs and OIDC state, with no key id. Rotating `NURTUO_AUTH_SECRET` (for example after a leak) logs out every user at once; there is no overlap period. Without `DATABASE_URL` the secret is random per process. | low | key management | `kid` header and a list of verification keys (current + previous) |
| 11 | Audit rows for logins have `workspace_id = NULL` and anonymous 401s are not recorded; a workspace owner cannot see failed logins against their members through the API. | low | audit | instance-level audit endpoint for an instance admin role |
| 12 | Deleting a workspace cascades `api_tokens` and `memberships`; the audit rows remain but point at token ids that no longer exist, so "which token did this" cannot be resolved later. | low | audit | soft-delete tokens and workspaces, or copy token name and prefix into audit `metadata` at write time |
| 13 | SAML and SCIM stub routes are `access: "public"` in `POLICIES`. If an implementer replaces the 501 handler without changing the policy, the SCIM endpoints are open to anonymous callers. | medium | future work | draft `enterprise-identity-saml-scim` defines a `scim` principal; add a test that SCIM routes reject anonymous callers once implemented |
| 14 | No MFA: a phished password is enough to get an owner session that can deploy to every runtime. | high | authentication | draft `enterprise-identity-saml-scim`, MFA phase |
| 15 | `lint` in `core/control-plane/package.json` is `echo 'lint: TODO (eslint)'`; the only static guard for secret comparison is the grep test in `crypto.test.ts`. | low | quality | draft `quality-gates` |

## Improvement opportunities

- Hash-chain audit rows (`prev_hash`) so a database operator cannot silently rewrite history after
  dropping the triggers.
- Short-lived access JWT plus refresh, or sliding session expiry with an absolute cap.
- Instance-level roles (for example `instance:admin`) separate from workspace roles, needed for user
  management and for gating workspace creation.
- Custom roles per workspace (roles table already exists; `builtin` flag is always true).

## References

- Open Mercato: `two-factor-authentication`, `rate-limiting`, `auth-acl-change-audit-logging`,
  `bearer-logout-session-revocation`, `tenant-ownership-and-module-acl-authorization`.

## Changelog

- 2026-11-22: created as built from https://github.com/nurtuo/nurtuo/pull/27 and https://github.com/nurtuo/nurtuo/pull/39.
