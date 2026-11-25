<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Authentication, authorization and audit (control plane)

Code: `core/control-plane/src/auth/`. Everything described here ships in the AGPL core: RBAC, SSO,
audit and API tokens are not feature-gated ("everything free").

## Integration with the API

`createHandler(repo, { auth })` takes either an auth hook (`createAuthHook({ service, oidc? })`) or the
literal `"disabled"`. `"disabled"` exists only for tests and local development: every route is open.
`main.ts` refuses to start without auth unless `NURTUO_INSECURE_NO_AUTH=1` is set, and `createHttpServer`
requires the option explicitly. The hook wraps the router, so `api.ts` itself contains no auth logic.

`main.ts` environment: `NURTUO_AUTH_SECRET` (>= 32 bytes, required with `DATABASE_URL`),
`NURTUO_BOOTSTRAP_EMAIL` / `NURTUO_BOOTSTRAP_PASSWORD` (creates the first owner and workspace `default`;
`NURTUO_BOOTSTRAP_EMAIL` alone also makes that email an instance admin),
`NURTUO_INSTANCE_ADMINS` (comma-separated instance-admin emails), `NURTUO_ALLOW_WORKSPACE_CREATE`
(`admins`, the default, or `any`; see [Instance admins](#instance-admins)),
`NURTUO_TRUST_PROXY=1` (use `X-Forwarded-For` for audit/rate-limit IPs; only behind a trusted proxy).

## Request pipeline

1. Match the route against `policy.ts` (method + path pattern). No match: 401 if anonymous, else 403 `no_policy` (fail closed).
2. Authenticate (public routes skip this): `Authorization: Bearer nrt_...` (API token), `Authorization: Bearer <jwt>`, or the session cookie. Presented-but-invalid credentials are 401, never "anonymous".
3. Cookie-authenticated mutating requests must carry `X-Nurtuo-Csrf` equal to the `csrfToken` returned at login (compared in constant time).
   Session-only routes (`POST /v1/workspaces`, `GET /v1/workspaces`) refuse API tokens with 403 before this step.
   Instance-level routes (`access: "instance"`, today only `POST /v1/workspaces`) then check the instance permission (see [Instance admins](#instance-admins)): 403 `forbidden` "missing permission instance:workspace_create", audited, before the request body is validated.
4. Resolve the workspace: `/v1/workspaces/{ws}/...` from the path (non-members get 404, so existence is not disclosed); other routes from `X-Nurtuo-Workspace` (id or slug), the token's own workspace, or the caller's only membership.
5. `authorize(permission, resource)` checks the permission inside that workspace. Denials are 403.
6. Handler runs (auth-owned routes in `endpoints.ts`, everything else falls through to `api.ts`).
7. Audit: every mutating request by an authenticated caller is appended to `audit_log`, including 4xx results and denials. If the audit write fails the request fails closed with 500 `audit_unavailable`.

`policy.test.ts` parses `openapi.yaml` and fails if any documented route has no policy, if a policy is
not documented, if a non-public operation lacks `401`/`403` responses, or if an anonymous request to any
route is not rejected. Adding a route to `api.ts` therefore requires an explicit authorization decision.

## Roles and permissions

Permissions are `resource:action`. Built-in roles (source of truth: `permissions.ts`; mirrored in
migration `0103_auth_seed_roles.sql`, a test fails on drift):

| role | intent |
| --- | --- |
| owner | everything, including deleting the workspace and managing owners |
| admin | everything except `workspace:delete`; cannot grant or touch owners |
| editor | read plus `component:write/delete`, own tokens |
| viewer | read-only |
| deployer | read-only plus `deployment:create`, own tokens |
| runtime | machine role: register runtimes, read desired state, report deployment status |

Escalation rule: a principal may grant, change or remove a role only if it holds every permission of
that role (`canGrant`). It is role-agnostic: admin cannot mint owners, editors cannot mint admins,
nobody can touch a role above their own. The last owner cannot be demoted or removed (atomic in the repository).

## Instance admins

Creating a workspace (`POST /v1/workspaces`) needs the instance-level permission
`instance:workspace_create`. It is not a workspace permission: no workspace role holds it, it is not an
API-token scope and `GET /v1/roles` does not list it (code: `src/auth/instance.ts`).

- Instance admins are the emails in `NURTUO_INSTANCE_ADMINS` (comma-separated; trimmed, case-insensitive)
  plus `NURTUO_BOOTSTRAP_EMAIL` whenever it is set. The caller's email is checked on each request, so an
  account created later with a listed email counts without a restart; removing an email (including
  unsetting `NURTUO_BOOTSTRAP_EMAIL` without listing it) takes effect on restart.
- `NURTUO_ALLOW_WORKSPACE_CREATE=admins` (default) allows only instance admins. `any` lets every session user
  create a workspace (the behaviour before this gate); the control plane logs a warning on every start.
- A malformed email or an unknown mode stops the control plane at start (exit 1). With no admins configured
  it starts with a warning and nobody can create workspaces. Give every replica the same values.
- API tokens can never create workspaces (session users only).
- Only an instance admin may create a new account for an instance-admin email through
  `POST /v1/workspaces/{ws}/members` (otherwise 403 `role_escalation`), so a workspace admin cannot
  pre-register that email with a password of their choice. Adding an existing account is unchanged.
- Until workspace data is partitioned (#55, see Known gaps), owning any workspace means full data access
  over every row; make only trusted people owners of workspaces you create.

The env-var list is temporary for phase 1. The target model is instance roles stored in the database and
managed through the API, tracked in [#77](https://github.com/nurtuo/nurtuo/issues/77).

## Sessions

Login verifies a scrypt hash (`scrypt$N$r$p$salt$hash`, N=16384 by default, NFKC-normalised, 12 to 256 chars),
creates a `sessions` row and sets `__Host-nurtuo_session` (HttpOnly, Secure, SameSite=Strict, Path=/). The
cookie holds an HS256 JWT (pinned algorithm, `iss`/`aud`/`exp` checked) whose `sid` must reference a live,
unrevoked session of an active user, so logout and disabling a user take effect immediately.
Failed logins: per-IP fixed window (30/min), per-account lockout after 5 consecutive failures for 15 minutes
(persisted on the user row; unknown emails are tracked in memory so they behave identically), uniform 401
message, one scrypt run per attempt regardless of account existence.

## API tokens

Format `nrt_<12 hex prefix>_<43 char secret>`. Only `sha256(secret)` is stored, looked up by prefix and compared
in constant time (an unknown prefix goes through the same comparison path). Tokens are bound to one workspace and
carry scopes (permissions). Effective permissions = scopes intersected with the creator's current role there, so
demotion shrinks a token and removing the creator from the workspace (or disabling them) kills it. Creating a
token requires every scope to be held by the caller (`scope_exceeds_permissions` otherwise), and a token-created
token cannot outlive its parent. Default lifetime 90 days, maximum 365. `last_used_at` is updated at most once a minute.

## Audit log

`audit_log` columns: actor (type and id), action, resource, resource id, `before_hash`, `after_hash`
(sha256 of canonical JSON, never the content), ip, request id (`X-Request-Id` if well-formed, else generated),
status, metadata, timestamp. Triggers block UPDATE, DELETE and TRUNCATE; rows are not foreign-keyed so they
survive workspace and user deletion. Query: `GET /v1/workspaces/{ws}/audit` with `actor`, `action`, `resource`,
`resourceId`, `status`, `from`, `to`, `limit` (1 to 200), `cursor`; newest first. Login attempts are audited with
a null workspace (visible to DB operators, not through the workspace endpoint). Anonymous 401s are not audited
(log flooding); plaintext tokens and passwords never reach the log.

## OIDC (implemented)

`OidcClient` (interface `OidcProvider`): authorization code with PKCE S256, `state` and `nonce` carried in a
short-lived HMAC-signed `__Host-nurtuo_oidc` cookie (SameSite=Lax, 10 minutes). The ID token is verified locally
against the issuer JWKS: RS256 only (`none`/HS* rejected), `iss`, `aud` (+`azp`), `exp`, `nonce`. Account mapping:
`(issuer, sub)` identity first; otherwise optional `linkByVerifiedEmail` (off by default, requires `email_verified`)
or JIT provisioning into a configured workspace with a role that can never be owner/admin. Tests use a fake IdP
(`oidc.test.ts`). Routes: `GET /v1/auth/oidc/login`, `GET /v1/auth/oidc/callback`.

## SAML and SCIM (design only, routes return 501)

`enterprise-stubs.ts` defines `SamlProvider` and `ScimBackend`; every route exists, is documented in
`openapi.yaml` and answers 501 (`application/scim+json` for SCIM). No SAML cryptography is implemented.
Design notes for the real implementation:

- SAML SP: SP-initiated only first. Do not hand-roll XML-DSig: use a maintained library or a separate
  sidecar, require signed assertions (or signed response), verify audience, `NotOnOrAfter`, `InResponseTo`
  (store request ids, one-time), reject unsigned and wrapped assertions, disable DTDs and external entities,
  pin the IdP certificate from metadata. Map NameID/email like OIDC (identity table with provider `saml:<entityID>`).
  `RelayState` must be bound to the browser the same way as the OIDC state cookie.
- SCIM 2.0: separate bearer token type (`scim:*` scope, not a user role), `/Users` and `/Groups` per RFC 7644,
  groups map to workspace roles through an explicit mapping table (never auto-grant owner). Deprovisioning
  (`active=false` or DELETE) disables the user, revokes sessions and tokens, and keeps audit rows. Provisioning
  actions are audited with actor type `token`.

## Known gaps

- Components, runtimes and deployments are not yet partitioned by workspace: the repository is global, so
  authorization decides who may call an operation in a workspace but all workspaces still see the same
  rows. A `workspace_id` column on those tables is the next migration; until then treat a control plane as
  single-tenant (one workspace) in practice.
- Audit is written after the action (no transactional outbox), so a crash between them loses an entry, and an
  audit failure returns 500 after the mutation was applied. No hash chaining of audit rows.
- Rate limiter and anonymous-email lockout are in-process; behind several replicas put a shared limiter in front.
  Per-account lockout lets an attacker lock a known account out (denial of service) for 15 minutes.
- No password reset, MFA, email verification or self-service signup; admins set initial passwords.
- The JSON body size is not bounded by the HTTP adapter, and there is no TLS termination (run behind a proxy).
- Session JWT in `Authorization: Bearer` is accepted for convenience; it is revocable but long-lived (8 h).
