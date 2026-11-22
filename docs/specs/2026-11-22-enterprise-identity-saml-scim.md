<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Enterprise identity: OIDC wiring, MFA, SAML SSO and SCIM provisioning

Status: draft. Date: 2026-11-22.

## TLDR

The README promises "SSO/SAML, SCIM ... included. No paid edition". Today OIDC works only for code
that embeds the package, SAML and SCIM routes answer 501 (`core/control-plane/src/auth/enterprise-stubs.ts`),
and there is no second factor. This spec delivers, in four phases: OIDC configurable in the shipped
binary, MFA (TOTP + recovery codes, WebAuthn later), SAML 2.0 SP-initiated SSO on a maintained
library, and SCIM 2.0 user/group provisioning with its own token type. All free, all in the AGPL core.

## Status and links

| | |
|---|---|
| Pull requests | none yet |
| ADRs | new ADR "SAML library choice and XML hardening" (phase 2) |
| Issues | none |
| Code | `core/control-plane/src/auth/{enterprise-stubs,stub-routes,endpoints,policy,service,oidc}.ts`, `core/control-plane/src/main.ts`, `core/control-plane/openapi.yaml`, new migrations |
| User docs | [auth.md](../auth.md#saml-and-scim-design-only-routes-return-501), [security.md](../security.md) |
| Evidence | `enterprise-stubs.ts` (`notImplemented`), `README.md` line "Everything free"; as-built `auth-rbac-audit` gaps 5, 13, 14 |

## Problem and goals

- Phase 0, OIDC wiring: `OidcClient` and the `auth.oidc_login`/`auth.oidc_callback` endpoints are
  implemented and tested (`oidc.test.ts`), but `main.ts` calls `createAuthHook` without `oidc`, so the
  shipped control plane answers 404 `OIDC is not configured`.
- Phase 1, MFA: a phished password gives a full owner session (8 h) that can deploy to every runtime.
- Phase 2, SAML: many enterprises only offer SAML to vendors. `SamlProvider` exists as an interface;
  every route returns 501.
- Phase 3, SCIM: without provisioning, leavers keep their nurtuo account until someone removes them
  by hand. `ScimBackend` exists as an interface; every route returns 501 with
  `application/scim+json`.

Goals: each phase ships independently with tests; no hand-written XML signature code; every
identity event audited; deprovisioning revokes sessions and tokens immediately.
Non-goals: acting as an IdP, IdP-initiated SAML in the first SAML release, SAML single logout, SCIM
`/Bulk` and `/Schemas` extensions beyond what Okta and Entra ID require, LDAP.

## Design

### Phase 0: OIDC in the shipped binary

`main.ts` builds `OidcClient` when `NURTUO_OIDC_ISSUER` is set:

| Variable | Meaning |
|---|---|
| `NURTUO_OIDC_ISSUER` | issuer URL, must be https (enforced by `OidcClient`) |
| `NURTUO_OIDC_CLIENT_ID`, `NURTUO_OIDC_CLIENT_SECRET` | client credentials (secret optional for public clients with PKCE) |
| `NURTUO_OIDC_REDIRECT_URI` | `https://<host>/v1/auth/oidc/callback` |
| `NURTUO_OIDC_JIT_WORKSPACE`, `NURTUO_OIDC_JIT_ROLE` | optional JIT; role must not be `owner`/`admin` (already enforced) |
| `NURTUO_OIDC_LINK_BY_VERIFIED_EMAIL` | `1` to enable `linkByVerifiedEmail` (off by default) |

Also: after a successful callback, redirect (302) to `NURTUO_PUBLIC_URL` instead of returning JSON,
since the callback is reached by a browser; the JSON body stays available with `Accept: application/json`.
Discovery is cached for the process lifetime today; add a 24 h refresh so IdP key rotation does not
need a restart (JWKS is already fetched per login).

### Phase 1: MFA

- Factors: TOTP (RFC 6238, SHA-1, 6 digits, 30 s, plus or minus 1 step) and 10 single-use recovery
  codes. WebAuthn/passkeys as phase 1b.
- Storage, migration `0105_auth_mfa.sql`:
  `user_mfa(user_id PK, totp_secret_enc bytea, enabled_at, last_used_step bigint)`,
  `user_recovery_codes(user_id, code_hash text, used_at, PRIMARY KEY(user_id, code_hash))`.
  The TOTP secret is encrypted with a key derived from `NURTUO_AUTH_SECRET` via HKDF
  (`info = "nurtuo-mfa-v1"`), AES-256-GCM. Recovery codes are stored as scrypt hashes.
  `last_used_step` rejects replay of a code within its window.
- Flow: `POST /v1/auth/login` with a correct password for a user with MFA returns 200
  `{ mfaRequired: true, mfaToken }` and **no session cookie**. `mfaToken` is an HS256 JWT with
  `aud: nurtuo-mfa`, `sub`, 5-minute expiry, single use (its `jti` is recorded). `POST /v1/auth/mfa/verify`
  `{ mfaToken, code }` starts the session. Wrong codes count towards the `login.account_ip` limit of
  `auth-rate-limiting-and-lockout`.
- Enrolment: `POST /v1/auth/mfa/totp` returns `otpauth://` URI and secret; `POST /v1/auth/mfa/totp/confirm {code}`
  enables it and returns recovery codes once. `DELETE /v1/auth/mfa` requires a fresh code. All
  audited (`auth.mfa_enroll`, `auth.mfa_disable`, `auth.mfa_verify`).
- Policy: workspace setting `requireMfa` (owner-managed, `workspace:manage`). A member without MFA in
  such a workspace gets 403 `mfa_required` on that workspace's routes until enrolled. OIDC and SAML
  logins are trusted to the IdP's MFA (`amr`/`acr` claims recorded in audit metadata, optional
  `NURTUO_OIDC_REQUIRE_ACR`).
- API tokens are not affected (they are not passwords); minting a token from a session requires that
  the session passed MFA when the workspace requires it.
- Admin reset: `DELETE /v1/workspaces/{ws}/members/{userId}/mfa` for `member:manage` with the
  `canGrant` rule; audited.

### Phase 2: SAML 2.0 SP

- Library, not hand-rolled XML-DSig: candidates `@node-saml/node-saml` (maintained fork of passport-saml)
  or a sidecar. The ADR records the choice, the version pin and the hardening below.
- Configuration per workspace (table `saml_connections(id, workspace_id, idp_entity_id, idp_sso_url, idp_cert_pem[], sp_entity_id, default_role, created_at)`),
  managed through `POST/GET/DELETE /v1/workspaces/{ws}/sso/saml` (`workspace:manage`). Several IdP
  certificates are allowed for rotation.
- Routes (already in `openapi.yaml` and `POLICIES`): `GET /v1/auth/saml/login?connection=<id>`
  (AuthnRequest, HTTP-Redirect binding), `POST /v1/auth/saml/acs` (HTTP-POST binding),
  `GET /v1/auth/saml/metadata?connection=<id>`.
- Validation (each one a test): signed Response or signed Assertion required (reject unsigned);
  signature checked against the pinned IdP certificate only, never a `KeyInfo` certificate from the
  message; reject XML signature wrapping (exactly one Assertion, the signed one); `Audience` equals
  SP entity id; `Destination` and `Recipient` equal the ACS URL; `NotBefore`/`NotOnOrAfter` with 60 s
  skew; `InResponseTo` matches an AuthnRequest id issued by us within 10 minutes and is consumed
  once (table `saml_requests(id, connection_id, expires_at, used_at)`); DTDs and external entities
  disabled; maximum `SAMLResponse` size 256 KiB. `RelayState` is bound to the browser with a signed
  `__Host-nurtuo_saml` cookie like the OIDC state.
- Account mapping: identity `saml:<idp_entity_id>` + `NameID` in `external_identities` (existing
  table). JIT into the connection's workspace with `default_role` (never owner/admin, same rule as
  OIDC). Email linking only with an explicit connection flag.
- Policies: SAML routes stay `access: "public"`; they are credential endpoints and get `LIMITS`
  entries (`saml.ip`).

### Phase 3: SCIM 2.0

- Principal: a new credential type, SCIM token `nrt_scim_<12 hex>_<43 base64url>`, stored in
  `scim_tokens(id, workspace_id, prefix, hash, created_by, created_at, revoked_at)`. It is not a user
  and has no role; `AuthService.authenticate` checks the `nrt_scim_` prefix before the `nrt_` API
  token prefix and returns `{ kind: "scim", workspaceId }`. Policies for `/v1/scim/v2/*`
  change from `pub(...)` to a new access kind `scim` that accepts only that principal and binds the
  workspace from the token. A policy test asserts anonymous and user/API-token callers get 401/403 on
  every SCIM route.
- Management: `POST/GET/DELETE /v1/workspaces/{ws}/scim/tokens` (`workspace:manage`), token shown once.
- Resources (RFC 7643/7644): `/Users` (list with `filter=userName eq "..."`, `startIndex`, `count`;
  get; create; replace; patch `active`, `emails`, `name`; delete), `/Groups` (list, get, create,
  patch `members`, delete), `/ServiceProviderConfig`, `/ResourceTypes`, `/Schemas` (static).
- Mapping: SCIM user = nurtuo user (`external_identities` provider `scim:<workspace_id>`, subject =
  SCIM `id`, `externalId` kept). Group to role through `scim_group_roles(workspace_id, group_id, role)`,
  set by workspace admins, never `owner`; a user's role in the workspace is the highest mapped role of
  their groups, recomputed on every group change. Users without a mapped group get no membership.
- Deprovisioning (`active=false` or DELETE): remove the membership in that workspace, revoke the
  user's API tokens there (as `removeMember` does), and if the user has no other memberships set
  `status = 'disabled'` and call `revokeSessionsForUser`. Audit rows are kept (no FK), and each SCIM
  write is audited with `actor_type = 'token'`, `metadata.scim = true`.
- Errors use `application/scim+json` with `urn:ietf:params:scim:api:messages:2.0:Error` (as the stubs
  already do).

## Contracts

| Contract | Phase | Change | Stability |
|---|---|---|---|
| Environment `NURTUO_OIDC_*`, `NURTUO_PUBLIC_URL` | 0 | new | experimental |
| `GET /v1/auth/oidc/callback` | 0 | 302 to `NURTUO_PUBLIC_URL` for browsers | experimental |
| `POST /v1/auth/login` | 1 | may return `{ mfaRequired, mfaToken }` without a cookie | experimental, client change needed |
| `POST /v1/auth/mfa/verify`, `/v1/auth/mfa/totp[/confirm]`, `DELETE /v1/auth/mfa`, `DELETE /v1/workspaces/{ws}/members/{userId}/mfa` | 1 | new | experimental |
| Error codes `mfa_required`, `mfa_invalid` | 1 | new | experimental |
| `/v1/auth/saml/*` | 2 | 501 replaced by implementation; `connection` query parameter | experimental |
| `/v1/workspaces/{ws}/sso/saml` | 2 | new | experimental |
| `/v1/scim/v2/*` | 3 | 501 replaced; requires SCIM token | experimental |
| SCIM token format `nrt_scim_<12 hex>_<43 base64url>` | 3 | new; does not match `parseApiToken` | stable intent |
| `/v1/workspaces/{ws}/scim/tokens`, `scim_group_roles` management | 3 | new | experimental |
| SQL: `user_mfa`, `user_recovery_codes`, `saml_connections`, `saml_requests`, `scim_tokens`, `scim_group_roles` | 1 to 3 | new migrations | internal |

## Compatibility

- Phase 1 changes the login response for MFA users only; `cli`, `core/web` and `sdk/ts/client` must
  handle `mfaRequired` before MFA can be enabled for anyone. Users without MFA see no change.
- Phase 3 changes SCIM routes from public to SCIM-token-only; nothing depends on the 501 stubs.
- `openapi.yaml` grows; `policy.test.ts` forces every new route to have a policy.

## Security

- No XML signature code in this repository (phase 2 ADR). Fuzz the ACS endpoint with the library's
  known wrapping and comment-injection payloads.
- TOTP secrets encrypted at rest; recovery codes hashed; MFA tokens single use and short-lived.
- SCIM tokens can create users and assign mapped roles, so they are workspace-scoped, cannot map
  `owner`, and their use is audited.
- JIT and group mapping can never produce `owner` or `admin` without an explicit admin-set mapping
  (and never `owner`).

## Testing

| Test | Phase | Proves |
|---|---|---|
| `main: NURTUO_OIDC_* configures the hook; missing redirect URI fails start-up` | 0 | wiring |
| `mfa: password-only login of an MFA user sets no cookie and returns mfaToken` | 1 | |
| `mfa: TOTP code accepted once, replay in the same step rejected` | 1 | |
| `mfa: recovery code works once` | 1 | |
| `mfa: mfaToken cannot be used as a session JWT and expires after 5 minutes` | 1 | audience separation |
| `mfa: requireMfa workspace returns 403 mfa_required for unenrolled members` | 1 | |
| `saml: valid signed assertion logs in and JIT-provisions with default_role` | 2 | |
| `saml: unsigned, wrapped, wrong audience, expired, replayed InResponseTo, KeyInfo-substituted cert are all rejected` | 2 | one case per attack |
| `saml: response over 256 KiB is 413` | 2 | |
| `scim: anonymous, user session and API token are refused on every SCIM route` | 3 | policy change |
| `scim: create user, add to mapped group, member gets mapped role` | 3 | |
| `scim: active=false removes membership, revokes tokens and sessions, keeps audit` | 3 | deprovisioning |
| `scim: group mapped to owner is refused` | 3 | |
| `scim: Okta and Entra ID request fixtures round-trip` | 3 | interop (recorded fixtures in `testdata/scim/`) |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A SAML library CVE (signature wrapping) lets an attacker forge an assertion for any email and get JIT access. | high | SAML | pin and monitor the library (Dependabot, `pnpm audit`), keep JIT off by default for SAML, require signed assertions, run the attack test corpus in CI |
| 2 | An admin maps an IdP group that everyone belongs to (for example "All Employees") to `admin`. | medium | SCIM | mapping UI warns on large groups; mapping changes audited; `owner` never mappable |
| 3 | TOTP secrets are encrypted with a key derived from `NURTUO_AUTH_SECRET`; rotating that secret makes every enrolled factor unreadable. | medium | MFA | key id on `user_mfa`, decrypt with previous keys during rotation (same `kid` list as `auth-rbac-audit` gap 10) |
| 4 | Users lose their device and recovery codes; only an admin can reset, and the last owner has nobody to ask. | medium | MFA | documented break-glass: `nurtuo-control-plane mfa reset <email>` CLI that requires database access, audited |
| 5 | The README promise "SSO/SAML, SCIM included" stays false until phase 2 and 3 ship. | medium | product | until then `docs/auth.md` and `security.md` state OIDC-only; README wording reviewed in the docs PR of phase 0 |

## References

- Open Mercato: `two-factor-authentication`, `oauth2-grant-lifecycle-core`.
- RFC 6238 (TOTP), RFC 7643 and RFC 7644 (SCIM 2.0), OASIS SAML 2.0 Core and Bindings.
- As-built spec `auth-rbac-audit`; draft `auth-rate-limiting-and-lockout` (limits for MFA and SAML).

## Changelog

- 2026-11-22: created (draft).
