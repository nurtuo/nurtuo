<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Rate limiting and account lockout for credential endpoints

Status: draft. Date: 2026-11-22.

## TLDR

Replace the in-process login throttle with a declared, per-route limit policy (login, token use,
token minting, OIDC callback), keyed on client IP and on identity, stored in Postgres so it holds
across replicas, answered with `429` + `Retry-After` + `RateLimit-*` headers. Fix client IP
derivation behind proxies, and make per-account lockout resistant to an attacker who only knows an
email address.

## Status and links

| | |
|---|---|
| Pull requests | none yet |
| ADRs | none needed unless a shared store other than Postgres is chosen |
| Issues | none |
| Code | `core/control-plane/src/auth/{throttle,service,middleware,endpoints,policy}.ts`, new migration in `core/control-plane/migrations/` |
| User docs | [auth.md](../auth.md#sessions), [auth.md](../auth.md#known-gaps) |
| Evidence | `throttle.ts` header ("Single-instance only"); as-built `auth-rbac-audit` gaps 2, 7, 8 |

## Problem and goals

As built (`auth-rbac-audit`):

- `AuthService` owns one `RateLimiter` (fixed window, 30/min per IP, `loginsPerMinutePerIp`) used by
  `login` and by the `auth.oidc_callback` endpoint, and a `FailureTracker` for unknown emails. Both
  live in process memory (`throttle.ts`), capped at 10,000 keys with lazy eviction.
- Per-account lockout is persisted (`users.failed_logins`, `users.locked_until`; 5 failures, 15 min),
  keyed only on the account.
- `clientIp` in `middleware.ts` takes the **first** `X-Forwarded-For` entry when
  `NURTUO_TRUST_PROXY=1`.
- API token and JWT verification failures are not throttled at all. Token minting is not limited.
- 429 responses carry `Retry-After` only from `auth.login` (the endpoint sets the header itself).
  A `RateLimited` thrown from `auth.oidc_callback` reaches the hook's generic `ApiError` handler, which
  drops `retryAfterSec`, so that 429 has no `Retry-After`, and because the exception bypasses the
  endpoint's audit return value, the throttled callback is not audited either.

Concrete failures this causes:

1. Behind a proxy that appends to `X-Forwarded-For`, a client that sends its own random header value
   gets a fresh IP bucket per request: unlimited password guessing (bounded only by per-account
   lockout) and forged IPs in `audit_log.ip`.
2. With N replicas the limit is N times higher; a restart forgets every IP window and every
   unknown-email lockout.
3. Five wrong passwords for `owner@example.com` from anywhere lock the owner out for 15 minutes,
   repeatable forever (denial of service with no credentials).
4. A leaked token prefix can be probed without limit (the 256-bit secret makes guessing infeasible,
   but every attempt costs a database lookup, so floods are a cheap DoS).

Goals: one declared limit table, correct IP derivation, shared storage, lockout that slows attackers
without letting them lock out a known user from a different network, standard headers, tests with a
fake clock. Non-goals: a general API rate limit for every route (later; reverse proxies cover it),
captchas, IP reputation feeds, WAF features.

## Design

### Limit policy

New `src/auth/limits.ts` exports `LIMITS`, referenced by policy action name:

| Key name | Applies to (policy action) | Key | Limit | On exceed |
|---|---|---|---|---|
| `login.ip` | `auth.login` | client IP (`/64` for IPv6) | 20 per 1 min, 200 per 1 h | 429 `rate_limited` |
| `login.account_ip` | `auth.login` failures | normalized email + client IP | 5 failures, then exponential backoff 1, 2, 4 ... 15 min | 429 `account_locked` for that IP only |
| `login.account` | `auth.login` failures | normalized email | 50 failures per 1 h from all IPs | 429 `account_locked` for all IPs, audit `auth.lockout`, notify owners (later) |
| `oidc.ip` | `auth.oidc_login`, `auth.oidc_callback` | client IP | 30 per 1 min | 429 `rate_limited` |
| `token.fail_ip` | any route, bearer `nrt_` token or JWT that fails verification | client IP | 30 failures per 1 min | 429 `rate_limited` before the database lookup |
| `token.fail_prefix` | same | token prefix | 10 failures per 1 min | 429; prefix logged in audit metadata |
| `token.mint` | `token.create` | principal user id | 20 per 1 h | 429 `rate_limited` |
| `member.add` | `member.add` | principal user id | 50 per 1 h | 429 (slows mass account creation with a stolen admin session) |

Values are defaults; `AuthServiceOptions.limits` overrides them and `main.ts` reads
`NURTUO_RATE_LIMITS` (JSON, same shape) for operators. A policy test asserts that every `access: "public"`
route except the SAML/SCIM stubs has an entry in `LIMITS`.

Algorithm: sliding window counter (two fixed buckets weighted), which avoids the burst at window
boundaries of today's fixed window. Backoff state for `login.account_ip` stores `failures` and
`blocked_until`.

`login.account` replaces today's hard 5-failure lockout: an attacker who knows only an email can at
most block their own IP (`login.account_ip`); blocking the account for everyone needs 50 failures per
hour, which also raises an audit event the owner can see. `users.failed_logins` and `locked_until` stay
as the persisted state of `login.account`.

Unknown emails go through exactly the same keys and timings as real ones (as today), so lockout
behaviour does not reveal whether an account exists.

### Client IP

`clientIp(req)`:

- `NURTUO_TRUST_PROXY` becomes a hop count or CIDR list: `NURTUO_TRUSTED_PROXIES=10.0.0.0/8,192.168.0.0/16`
  or `NURTUO_TRUST_PROXY=1` meaning "one proxy". The client IP is the right-most `X-Forwarded-For`
  entry that is not a trusted proxy (RFC 7239 `Forwarded` is parsed too).
- If the socket peer is not a trusted proxy, forwarding headers are ignored.
- Invalid addresses fall back to the socket address. IPv6 is bucketed per `/64`.

### Storage

New migration `0104_auth_rate_limits.sql` (next free number in the auth range):

```sql
CREATE UNLOGGED TABLE auth_rate_limits (
  key          text PRIMARY KEY,          -- e.g. "login.ip:203.0.113.9"
  window_start timestamptz NOT NULL,
  count        integer NOT NULL,
  prev_count   integer NOT NULL DEFAULT 0,
  blocked_until timestamptz
);
```

- One `INSERT ... ON CONFLICT (key) DO UPDATE ... RETURNING` per check: atomic across replicas.
- `UNLOGGED`: counters may be lost on a Postgres crash, which is acceptable (fails open for at most
  one window, never locks users out permanently).
- Cleanup: `DELETE FROM auth_rate_limits WHERE window_start < now() - interval '2 hours' AND (blocked_until IS NULL OR blocked_until < now())`
  every 5 minutes from one replica (advisory lock).
- Interface `RateLimitStore { hit(key, limit, windowMs, now): Promise<{ allowed: boolean; remaining: number; resetSec: number }>; fail(...); clear(key) }`
  with `MemoryRateLimitStore` (today's classes, kept for tests and single-process dev) and
  `PgRateLimitStore`. `main.ts` picks Pg when `DATABASE_URL` is set.
- If the store is unreachable: credential endpoints fail closed with 503 `rate_limit_unavailable`;
  token verification falls back to the in-memory store (so an outage of the counter table does not
  take every API call down).

### Responses

- 429 body `{ "error": { "code": "rate_limited" | "account_locked", "message": "..." } }` (unchanged codes).
- Headers on every 429: `Retry-After: <seconds>` (integer). The hook maps `RateLimited` centrally, so
  every endpoint gets it, including the OIDC callback.
- On limited routes, also on success: `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`
  (IETF draft field names, integers). Not sent for `login.account*` keys, so remaining attempts per
  account are not disclosed.
- Audit: every 429 on `auth.login` and `auth.oidc_callback` is already audited as a failed login; add
  `metadata.limit` (key name, not key value) and a separate action `auth.lockout` when
  `login.account` trips.

### Admin unlock

`DELETE /v1/workspaces/{ws}/members/{userId}/lockout` (permission `member:manage`, `canGrant` rule
applies) clears `login.account*` state for that user. Audited.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| 429 responses | `Retry-After` on every 429; `RateLimit-*` on limited routes | experimental |
| Error codes | unchanged (`rate_limited`, `account_locked`); new `rate_limit_unavailable` (503) | experimental |
| New route | `DELETE /v1/workspaces/{ws}/members/{userId}/lockout` (in `openapi.yaml` and `POLICIES`) | experimental |
| Environment | `NURTUO_TRUSTED_PROXIES`, `NURTUO_TRUST_PROXY` (now a hop count; `1` keeps working), `NURTUO_RATE_LIMITS` | experimental |
| `AuthServiceOptions` | `limits`, `rateLimitStore`; `loginsPerMinutePerIp`, `maxFailures`, `lockMs` kept as aliases for one release | internal |
| SQL | `0104_auth_rate_limits.sql` | internal |

## Compatibility

- Clients see fewer lockouts for legitimate users (lockout is per IP first) and more 429s for API
  token floods. `NurtuoClient` in `sdk/ts/client/src/index.ts` already retries 429 for idempotent methods and honours
  `Retry-After` (`retryAfterMs`); `POST /v1/auth/login` is not idempotent, so the CLI shows the 429.
- `NURTUO_TRUST_PROXY=1` keeps meaning "trust one proxy" but now takes the right-most entry; setups
  with two chained proxies must set `NURTUO_TRUST_PROXY=2` or `NURTUO_TRUSTED_PROXIES`.

## Security

Fails closed for credential endpoints, fails open (in-memory) for token verification. Keys never
contain passwords or token secrets; the email part is stored as `sha256(email)` in the key to keep
the table free of personal data. Limits apply before scrypt runs, so a flood cannot burn CPU.

## Testing

Acceptance tests (fake clock from `test-util.ts`; Pg variants `DATABASE_URL`-gated and required in CI):

| Test | Proves |
|---|---|
| `limits: every public credential route has a LIMITS entry` | policy coverage |
| `ip: right-most untrusted X-Forwarded-For hop is used; spoofed left-most entries are ignored` | gap 1 fixed |
| `ip: forwarding headers ignored when the peer is not a trusted proxy` | |
| `login: 6th failure from one IP is 429 account_locked, same account from another IP still gets 401` | DoS resistance |
| `login: 50 failures from many IPs lock the account everywhere and audit auth.lockout` | distributed guessing |
| `login: unknown and known emails produce identical status codes and Retry-After values` | no enumeration |
| `oidc callback 429 carries Retry-After` | header regression |
| `token: 31st bad token from one IP within a minute is 429 without a repository lookup` | flood protection |
| `token.create: 21st mint in an hour is 429` | |
| `pg store: two AuthService instances on one database share counters` | multi-replica |
| `pg store unavailable: login is 503, token auth still works` | failure modes |
| `admin unlock clears lockout and is audited; editor cannot unlock an admin` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Users behind one corporate NAT share an IP; a misbehaving script on one laptop triggers `login.ip` for the whole office. | medium | availability | per-IP limit is generous (20/min); `login.account_ip` is the tight one; operators can raise `login.ip` |
| 2 | One extra database round trip per login and per failed token check. | low | performance | successful token checks are not counted; logins are rare |
| 3 | `UNLOGGED` table is emptied after a Postgres crash, so counters reset. | low | resilience | acceptable; documented |

## References

- Open Mercato: `rate-limiting`.
- As-built spec `auth-rbac-audit`.

## Changelog

- 2026-11-22: created (draft).
