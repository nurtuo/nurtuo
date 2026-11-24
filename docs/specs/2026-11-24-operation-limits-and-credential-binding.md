<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Operation-aware limits and stored-credential binding

Status: draft (needs owner decision). Date: 2026-11-24.

## TLDR

Replace the global 1 MiB request cap from the canvas Run spec with limits chosen per operation, and
check that control-plane SFTP credentials are bound to their authorized destination before they are
released. The first protection step is to authenticate before reading a body. The global cap is
not that step. Existing revision uploads keep working. Credential release is a server-side
authorization decision. The decisive test is that **an unauthorized peer sees zero authentication
attempts**.

This spec is the written outcome of a two-expert council (Claude Opus and GPT-6.2 Astra). The
council ran three rounds (Astra R1, Opus R2, Astra R3) and both experts say no further round is
needed. Three points stay divergent and are owner decisions: the revision ceiling (D2), the
override rule (D4) and `known_hosts` dialing (D12). Łukasz must decide D1-D14 before any code is
written, including the PR #90 rework.

## Status and links

| | |
|---|---|
| Pull requests | this spec (draft). Affects [PR #90](https://github.com/nurtuo/nurtuo/pull/90) (canvas Run implementation, head `aa02133`) |
| Reviews it answers | Codex review https://github.com/nurtuo/nurtuo/pull/90#issuecomment-7134020883, Astra review https://github.com/nurtuo/nurtuo/pull/90#issuecomment-7134187185, owner decision https://github.com/nurtuo/nurtuo/pull/90#issuecomment-7134459612 |
| Amends | [canvas Run](2026-11-23-canvas-run.md): D2 scope, Phase 2 body limit, D3/D6 secret workflow, Gaps #3, #7, #9 |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md) (connections and secrets), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (gateway), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) (engine) |
| Related specs | [backward compatibility contract](2026-11-22-backward-compatibility-contract.md), [auth rate limiting and lockout](2026-11-22-auth-rate-limiting-and-lockout.md), [bundle artifact delivery](2026-11-22-bundle-artifact-delivery.md), [HTTP listener routing](2026-11-22-http-listener-routing.md) |
| Code (main `fbdb9e8`) | `core/control-plane/src/api.ts` (`createHttpServer`, `canonicalJson`), `core/control-plane/src/auth/policy.ts` (`POLICIES`), `core/control-plane/src/auth/throttle.ts`, `core/control-plane/src/pg-repository.ts`, `core/control-plane/src/memory-repository.ts`, `core/runtime/internal/listener/http.go` (`MaxBodyBytes`), `core/runtime/internal/connections/connections.go` (`Resolver`) |
| Code (PR #90 `aa02133`) | `core/control-plane/src/testrun/runner.ts`, `core/control-plane/src/testrun/egress.ts`, `core/control-plane/src/connections/routes.ts`, `core/control-plane/src/connections/validate.ts`, `core/web/src/ui/ConnectionsPanel.tsx` |

## Owner requirement

Łukasz, 2026-11-24 15:29 Europe/Warsaw, recorded on PR #90 (verbatim):

> Nurtuo jest platformą integracji systemów, która musi obsługiwać duże payloady, rewizje, webhooki oraz masową wymianę danych. Zachowaj kompatybilność istniejącego API. Zaprojektuj konfigurowalne limity zależne od rodzaju endpointu i charakteru operacji. Uwzględnij streaming, ochronę pamięci, rate limiting oraz bezpieczeństwo. Nie wprowadzaj arbitralnych limitów bez analizy rzeczywistych wymagań platformy. Zleć Opusowi i Astrze uzgodnienie rozwiązania architektonicznego przed implementacją.

He rejected a global 1 MiB limit (PR #90 comment 7134459612: "no new 413s for current large revision
uploads").

## Problem and goals

Facts on `main` (`fbdb9e8`):

1. `createHttpServer` (`core/control-plane/src/api.ts`) collects every chunk, decodes UTF-8 and runs
   `JSON.parse` before it calls `handle()`. Authentication runs inside `handle()`. An
   unauthenticated client can therefore send a body of any size, on any path including unknown
   ones, for up to Node's default `requestTimeout` (300 s), and the process holds all of it. The
   same process hosts the runtime gateway (`main.ts`). This is the largest memory exposure today.
2. Implicit ceilings already exist and fail badly. A string above `buffer.constants.MAX_STRING_LENGTH`
   (536,870,888 UTF-16 units on this tree's Node) makes decoding throw inside the parse `try`, so a
   well-formed document gets 400 `invalid JSON`. Heap exhaustion usually comes earlier and kills the
   process. PostgreSQL `jsonb` has its own internal-size limit, which turns into a 500. That limit is
   not a wire-size bound (see V2).
3. `GET /v1/components/{id}/revisions` returns every revision with full content
   (`pg-repository.ts`, `SELECT *`). It is serialized with one `JSON.stringify`.
4. The runtime webhook listener streams through `http.MaxBytesReader` with a fixed 1 MiB default
   (`listener/http.go`, `MaxBodyBytes`). The factory does not set it and `nurtuo.flow/v1` has no field
   for it.
5. The runtime `connections.Resolver` replaces `secret://` in any string setting, not only in
   credential fields.

Facts on PR #90 (`aa02133`): connection and secret routes are new, experimental and unmerged. A
workspace editor can repoint a connection's host, user or trusted key while keeping its stored
password. The runner then reveals the password to the new server (Codex blocker). DNS preflight can
hold run/scan slots past the deadline (Codex major). Editing a connection can replace its password
reference and drop hidden settings (Codex major).

- Goals:
  - Limits are chosen by operation (method plus route template), not by one global cap.
  - No new rejection of a request that succeeds today on a STABLE route unless Łukasz explicitly
    approves it (D2, D3, D14).
  - Memory protection: no body is read before authentication, except on body-authenticated routes,
    which get a bounded parser. Large bodies and responses are reserved and streamed.
  - Rate limiting: per-peer pre-auth concurrency, login throttle (kept), and D2 run concurrency.
  - Security: a stored credential reaches only the destination and trust configuration it was
    entered for. `secret://` appears only in credential slots.
  - Default values come from platform constants or the approved D2 analysis, never from a percentile
    guess. Values without a basis are marked "initial, benchmark-gated".
- Non-goals:
  - Streaming revision storage redesign (L2). It gets its own spec and is triggered by metrics (D6).
  - Runtime webhook listener limits. A separate follow-up spec covers a per-trigger flow field and a
    `runtime.yaml` ceiling (V10).
  - Cross-replica quotas, byte-rate quotas, shared quota service. These belong to a later HA spec (D7).
  - A persistent encrypted secret store (canvas Run Gaps #4, #5).
  - Mutating SFTP operations, other connectors in Run (canvas D11 unchanged).
  - Reopening canvas Run decisions other than those named under "Amends".

## Decisions (pending)

Nothing below is approved. Each item names the recommended default. Letters in brackets map to the
council ledgers (`D-Lx` limits, `D-Sx` secrets). The "Council" column gives the outcome after
Round 3: **agreed**, **agreed with qualification** (the qualification is part of the recommended
default), or **divergent** (both positions given; Łukasz decides). See the Council section for each
V-item.

| # | Question | Council | Recommended default | Alternative |
|---|---|---|---|---|
| D1 [D-L1] | Global 1 MiB cap | agreed (V1) | Supersede the global Phase 2 cap and its test. D2's values (1 MiB input, 20 documents and 1 MiB total decoded output, 30/120 s, 2/workspace, 4/instance shared slots) apply to `POST …/test-runs` only. Scan has its own small profile (4 KiB body, 10 s, shares the run limiter) | keep global cap (rejected by owner) |
| D2 [D-L2] | Default body ceiling on revision routes | **divergent** (V2) | **No default byte ceiling** on `POST /v1/components/{id}/revisions`. Never a 413 on GET or list. Revision upload and full-content readback are preserved by default. An operator may configure a documented finite ceiling. Making one an upgrade default would need a separate compatibility decision. Memory is protected by D14 instead. Position of Astra (R1, R3), adopted by the drafter | explicit 256 MiB 413 on revision POST (Opus R2; the drafter withdraws it, see V2) |
| D3 [D-L3] | Bounded parser on body-authenticated routes | agreed (V1) | Approve in principle a bounded, memory-only parser for `POST /v1/auth/login` (and any other route whose authentication needs body fields). 64 KiB is a candidate, final value after field/encoding/client evidence. No blanket cap on public routes. Document the changed status and use the compatibility process for STABLE routes. The compatibility contract's emergency security exception applies only if its advisory and severity requirements are met | no change (unauthenticated body stays unbounded on login) |
| D4 [D-L4] | Configuration surface | agreed with qualification; **divergent** on the override rule (V3) | Operation-specific defaults in code, keyed by policy action, method/template coverage tested against OpenAPI, server-selected profiles enforced. Operator overrides **per class** via one EXPERIMENTAL surface (name chosen by Łukasz, e.g. `NURTUO_LIMITS_FILE`), with units, scope and defaults documented and validated at startup. Overrides may tighten (explicit operator restrictions), and may raise when evidence supports it, within the instance's physical capacity (Astra R3). Size the `secret-bearing` PUT for all allowed fields in escaped wire form plus settings. Benchmark scan size, parse expansion and reservation thresholds before setting new defaults | tighten-only, except a bounded raise of the revision ceiling (Opus R2) |
| D5 [D-L5] | Child output bound | agreed with qualification (Astra ledger) | Add EXPERIMENTAL `nurtuo flow run --max-output-bytes` and `--max-documents`, applied in the child before a whole payload is read or a result set accumulated, and counting metadata, encoded output and temporary disk. Overflow keeps the D2 failed result. Shared execution is enabled only with demonstrated producer bounds or equivalent isolation. Until it lands, #90 documents the residual risk (canvas Gaps #7) | accept the residual risk without a flag |
| D6 [D-L6] | Revision storage redesign (L2) | agreed with qualification (Astra ledger) | Separate spec, triggered by measured demand or by failure to meet the working-set budget. Before switching it must prove digest parity against `canonicalJson` (duplicate and prototype-named keys, UTF-16 ordering, UTF-8 replacements, lone surrogates, negative zero, overflowing numbers), bounded sorting, old-row readback, atomic publication and cleanup | include in first delivery |
| D7 [D-L7] | Rate limiting and quotas in M1 | agreed with qualification (V3, Astra ledger) | Instance-local admission and one volatile-secret owner. Keep login throttle/lockout, add a bounded per-peer pre-auth in-flight limit (trusted-proxy identity, bounded key cardinality), keep D2 concurrency. Reserve capacity for reads, writes and teardown. Metrics by operation (bytes, rates, CPU, RSS, spool, wait, rejection) with no bodies or identifiers. No byte-rate or workspace quotas; a later HA spec owns cross-replica fairness | add workspace/byte-rate quotas now |
| D8 [D-S1] | Credential write API | agreed (V5, V7); agreed with qualification on the publication rule (V6) | Optional write-only `credentials: {password?, privateKey?, passphrase?}` member on the pending `PUT /v1/workspaces/{ws}/connections/{id}`. Credential-bearing PUT needs `connection:write` and `secret:write`, a user session, CSRF and TLS. An unchanged-binding edit needs `connection:write` only. Hidden settings and server-owned references are preserved; client-selected substitutes are rejected. An omitted field is retained only when the binding is unchanged. A binding change with retained credentials returns **409 `credential_rebind_required` before any mutation**. Publication follows the V6 rule | dedicated credential-save operation; staged two-call handle (Astra R1, superseded by Astra R3) |
| D9 [D-S2] | Free-standing `/secrets` routes | agreed (V5) | Remove `GET …/secrets`, `PUT|DELETE …/secrets/{name}` from the pending API. Connections report `secrets: {field: set|missing}`. Intentional credential removal is specified on the connection with `secret:write`. Deleting a connection or workspace purges its credentials | keep them as storage that the runner never releases |
| D10 [D-S3] | `secret://` placement | agreed (Astra ledger) | Allowed only in schema credential slots (`writeOnly` / `x-nurtuo-secret`), server-assigned for this connection and field. Elsewhere 400 `secret_ref_not_allowed` on save, and refusal at snapshot time for a malformed stored row. All connections are validated before any child input is released. Applies to control-plane-managed Run connections only. Standalone providers, bundle syntax and `secret://` stay unchanged | none proposed |
| D11 [D-S4] | `rootPath` in the binding | agreed (V8) | Not bound; editors can change it. Documented as a flow-path convenience on an already authorized account, not an authorization boundary. A future subtree-authorization promise needs a separate decision | bind it |
| D12 [D-S5] | DNS rebinding / dial address | **divergent** on `known_hosts` (V9) | Pin a vetted dial address for `fingerprint` connections and scans, keeping the logical SSH identity for the binding and known-hosts verification, with the egress check at every actual dial (scan, retries, all address families). For `known_hosts`, an approved dial-address mechanism (a child flag, Ask First) or equivalent network enforcement is needed. Until then that mode is **not enabled on shared installations** (Astra R3, recommended: best-effort preflight leaves the zero-authentication test unenforceable) | `known_hosts` enabled as best effort and documented until a dial flag is approved (Opus R2) |
| D13 [D-S6] | Revocation | agreed (Astra ledger) | Delete or rebind revokes future acquisitions. An admitted run finishes under its D2 deadline plus bounded teardown grace (initial 5 s). Current permission is checked and the full snapshot is acquired at admission. UI logout revocation and failure reporting stay on #90 | kill in-flight runs |
| D14 | Large-body reservation and capacity error | agreed (V4) | A request or response above an in-memory threshold (initial 8 MiB, benchmark-gated) takes a large-body token (initial 1 per instance), reserved before allocating. Aggregate small-buffer and queue occupancy are capped too, because one token alone does not bound process memory. If no token is free before the deadline, the request gets **503 `capacity_unavailable` + `Retry-After`**. This is a new transient failure on STABLE routes, so it needs owner approval. 413 = applicable size limit, 429 = caller quota (D2 `too_many_runs` stays 429; a 503 there would need an explicit amendment), 503 = instance capacity | no reservation (OOM risk stays) |

## Design

### Architecture agreed by both experts

These points are agreed in Astra Round 1 and Opus Round 2 and confirmed in Astra Round 3 (see
Council).

1. **One operation inventory.** Admission uses the same method plus normalized route template table
   as authorization (`POLICIES` in `auth/policy.ts`). Every OpenAPI operation has an explicit limit
   profile. A test fails when one is missing, like `policy.test.ts`. Unknown routes are answered
   without reading the body. D9's 404 for workspace routes in auth-disabled mode stays.
2. **Authenticate before reading the body.** Policy match, cookie or bearer, CSRF header, workspace
   from path or `X-Nurtuo-Workspace`, and `authorize` need only headers and path. They run before any
   body read. Body-authenticated routes use their own bounded parser (D3). The OIDC callback is a
   GET and reads only the query string. SCIM routes run their own authorization before reading a
   mutation body. Authentication is separated from endpoint execution, so a public policy entry
   cannot bypass a route-specific check.
3. **Accepted payload is not resident working set.** Bytes are counted while reading, including
   chunked bodies. Content-Length allows early rejection where a limit exists. Parse cost, graph
   validation, output queues and retained records are accounted separately. A response is charged
   until the client drains it or disconnects.
4. **Concurrency reservations matter more than time-window rates.** A stalled request is not
   contained by a requests-per-second budget. Login throttle and lockout stay.
5. **End-to-end deadline for Run and scan.** The deadline starts before preflight. Capacity is
   reserved before expensive graph validation and DNS. Abort propagates through DNS, connect, child
   and response on deadline, disconnect or shutdown. Each reservation is released exactly once.
   Teardown (socket close, child reaped) is accounted for separately.
6. **Child memory.** Polling temp-dir size and capping stdout do not stop the Go child from
   materializing a payload (`writeOutput` calls `p.Bytes()`). The bound must be in the producer (D5)
   or in tested process isolation.
7. **Revision storage redesign is decoupled** (D6). Route isolation and Run controls ship without it.
8. **Measure before setting a new default on a stable operation.** Metrics: wire and decoded bytes,
   document counts, duration, parse CPU, peak RSS, spool high-water, concurrency, rejection cause,
   all by route template, with no bodies, no identifiers as labels and no secret data.
9. **Credential release is a server-side authorization decision**, made from the exact settings
   given to the child. It never depends on a secret name, naming convention, first use, or what a
   connection currently points at.
10. **The binding covers destination and trust**: workspace, connection incarnation, connector,
    credential field, logical host, effective port, exact SSH user, auth mode, host-key policy and
    complete trust material. A change in trust material requires fresh credential entry.
11. **Single-process owner of volatile secrets in M1.** A replica never substitutes a same-named local
    value. A restart that loses volatile material fails closed (`missing`, re-entry).
12. **No change to a released v1 route is needed for credential binding.** The connection and secret
    API exists only on #90 and is experimental.

### Operation classes and initial defaults

| Class | Operations | Body | Time / concurrency | Basis |
|---|---|---|---|---|
| `preauth` | `POST /v1/auth/login`, other body-authenticated routes | bounded, memory only, candidate 64 KiB (D3) | login throttle; per-peer in-flight limit (D7) | largest legitimate input is credentials and tokens |
| `control` | authenticated management JSON (component metadata, runtimes, deployments, members, tokens, workspaces, roles, SCIM) | no new default ceiling; D14 reservation above the threshold | listener-wide `headersTimeout`, `requestTimeout` (300 s kept), `maxHeaderSize` set explicitly | compatibility of STABLE routes |
| `document` | `POST`/`GET /v1/components/{id}/revisions`, `GET …/revisions/{n}` | no default ceiling (D2); D14 reservation; operator may set a ceiling | `requestTimeout` 300 s kept | owner requirement; whitespace and aggregate-list cases in V2 |
| `secret-bearing` | credential-bearing connection PUT (D8) | sized for all allowed fields at the existing `MAX_SECRET_BYTES` (64 KiB per decoded value, in #90) in escaped wire form plus settings; final envelope value after measurement | never staged to disk, traced or hashed | sensitive values of known size |
| `interactive-run` | `POST /v1/workspaces/{ws}/test-runs` | 1 MiB (D2), 413 | 30 s default, 120 s max; 2/workspace, 4/instance; 429 `too_many_runs` | approved D2 |
| `scan` | host-key scan | 4 KiB | 10 s; shares the run limiter | tiny request that opens a socket |

The 8 MiB threshold, token counts and the `secret-bearing` envelope are placeholders. L1 benchmarks
replace them, and each change is recorded in the changelog.

### Streaming

- **Request bodies.** L1 reads only after authorization. For `document` and `control`, bodies below
  the threshold stay in memory. Above it the request holds a D14 token for its whole lifetime. A
  true incremental parse of a revision is L2 (D6), because `canonicalJson` must keep its exact
  semantics (UTF-8 replacement before parse, last duplicate key wins, `__proto__` as own key,
  `Number`/`String` round trip, UTF-16 key order, lone surrogates escaped).
- **List response.** Writing the existing array one item at a time removes the one large string.
  The database result still stays resident. The full fix is a cursor or bounded iterator with a
  consistent read snapshot that keeps the `items` envelope, order and full content, honors
  backpressure and closes on disconnect. A failure after headers are sent must abort the stream. A
  partial list must never complete as a success. Phase L1b covers this. The memory budget also
  accounts for the largest materialized row, concurrent small requests, retained memory-repository
  history and slow consumers. Materialization is isolated where its measured peak cost exceeds the
  available headroom.
- **Webhooks** stay on the runtime listener, which already streams into the spool and HMAC-verifies
  the exact bytes before dispatch (V10).

### Credential binding (D8-D13)

- **Ownership.** For control-plane connections, credential slots are server-managed. A value is
  stored as an immutable version under `(workspaceId, connectionIncarnation, field, version)`.
  `connectionIncarnation` is a server-generated UUID added to the unmerged `0300` migration. It is
  regenerated on delete/recreate, so recreating the same id cannot reuse an old credential. The
  connection shows a read-only, server-assigned `secret://` name, so `nurtuo.connection/v1` files
  for the child are unchanged.
- **Binding digest.** Each version stores
  `binding = sha256(canonical({connector, field, incarnation, host, port, user, authMode, hostKeyPolicy, hostKeyMaterial}))`.
  Normalization covers only what the dialer and verifier both treat as equal: host lower-cased, a
  trailing dot dropped, no DNS resolution, port defaults to 22, user exact, `knownHosts` text as
  stored. Absolute DNS names are normalized only where lookup, dial and verification agree. Any
  mismatch fails closed. The digest hashes only non-secret settings, so it may be
  audited.
- **Publication and acquisition (V6).** A digest comparison alone is not enough. Two rotations at
  the same destination have the same digest, and a database commit followed by a map swap creates
  two visibility points. Rule: stage the new versions privately, then publish the connection
  settings together with the exact version references under a generation check (or a serialized
  writer). A run acquires one consistent generation: settings, version references and values
  together, in one store operation: checking the digest and acquiring that exact version is a single
  store operation, never a check followed by a lookup by name. It compares the digest computed from
  **the logical identity in the snapshot from which the child's connection file is derived** with
  each version's stored digest before `reveal`. The snapshot keeps the logical host and the vetted
  dial address separately (D12). The logical identity is bound before the dial address is derived;
  comparing the digest with a `fingerprint` child file after an IP was substituted would compare two
  different hosts. On mismatch: 409 `credential_unbound`, no child, no network contact. Prior versions stay alive while
  an acquired snapshot uses them. An uncertain database outcome is resolved by reading the
  committed generation before cleanup or retry. No lock is held across network execution. An
  optional `If-Match` on a server ETag prevents lost edits but is not needed for correctness:
  internal publication stays correct when the client omits it.
- **Edits.** A settings-only PUT with an unchanged binding needs `connection:write` and keeps every
  untouched setting (`timeoutMs`, `maxListEntries`, …) and the server-owned references. A
  client-selected substitute reference is rejected. A failed save leaves the prior settings and
  credential usable.
- **Slot confinement (D10).** Example exploit without it: connection B saved with
  `host: attacker.example`, `user: "secret://rebex-demo-password"` sends the victim's password as the
  SSH username. All connections in a run are validated before any child input is written. The child
  environment holds only bound, matched credentials of this run (D12 minimal env and collision checks
  kept).
- **Scan.** Acquires no credentials, uses the same egress check and dial address, and performs
  zero authentication.
- **Logging.** Values and secret-bearing bodies never reach logs, traces, audit hashes, files, argv,
  errors or previews. Audit records actor, operation, outcome, connection id and binding digest.

### Failure modes

| Case | Response |
|---|---|
| body over a class limit (`interactive-run`, `scan`, `preauth`, operator-set ceiling) | 413 `payload_too_large`, before parsing |
| unknown route | 404/405 as today, body not read |
| unauthenticated or unauthorized request with a body | 401/403 as today, body not read |
| no large-body token before the deadline | 503 `capacity_unavailable`, `Retry-After` (D14) |
| run/scan concurrency exhausted | 429 `too_many_runs` (D2, unchanged) |
| binding changed without fresh credentials on PUT | 409 `credential_rebind_required`, nothing mutated |
| stored binding does not match run snapshot | 409 `credential_unbound`, no child, no network contact |
| `secret://` outside a credential slot | 400 `secret_ref_not_allowed` on save; run refused for a stored row |
| DNS stalls past the deadline | run `failed` with timeout, slot released at the deadline |

## Contracts

| Contract | Change | Stability |
|---|---|---|
| STABLE control-plane routes | body read after auth; no new default 413; possible 503 `capacity_unavailable` (D14); login bounded (D3) | STABLE (D3, D14 need approval) |
| `POST …/test-runs`, host-key scan | D2 values scoped to these routes | experimental (pending, #90) |
| `PUT …/connections/{id}` | optional write-only `credentials` member; 409 `credential_rebind_required`; read-only server-assigned refs (D8) | experimental (pending, #90) |
| `…/secrets` routes | removed from pending API (D9) | experimental (pending, #90) |
| run error `credential_unbound` (409), save error `secret_ref_not_allowed` (400) | new | experimental |
| limits configuration surface (D4) | new, name chosen by owner | experimental |
| `nurtuo flow run --max-output-bytes`, `--max-documents` (D5) | new, additive | experimental |
| `secret://` syntax, `nurtuo.connection/v1`, bundle syntax | unchanged | frozen / stable |

## Compatibility

- Revision create, get and list keep today's accepted sizes and full content (D2). The canvas Run
  spec's test "body over 1 MiB is 413" on all routes is replaced by per-route tests.
- Login: a body over the D3 limit, which succeeds today, would get 413. This is handled as the
  compatibility contract's security exception and needs D3.
- D14 adds a transient 503 that clients can retry. It needs approval because a STABLE route could
  return it where it succeeded before (two concurrent large uploads).
- Connection and secret routes are unmerged, so D8/D9 do not affect released clients. They do change
  approved canvas Run D3/D6, the pending OpenAPI, `@nurtuo/client` generated types and the UI tests,
  which change together in #90. Any trial data with unbound credentials is treated as `missing` and
  needs re-entry.

## Security

- Pre-auth memory: no body read before authorization except bounded D3 parsers. Per-peer in-flight
  limit uses `NURTUO_TRUST_PROXY` semantics.
- Credential binding as above. TLS is required for credential-bearing PUTs, as for login.
- SSRF: D11 of canvas Run unchanged. Egress is checked at the real dial, for retries and all address
  families (D12).
- Large-body staging, if L2 adds it later, holds customer content and needs private access, bounded
  retention and cleanup after crashes.

## Implementation plan

No phase starts before Łukasz approves this spec. Each phase is a separate PR with its tests.

### Phase P90: PR #90 rework (must land in #90)

Findings from the Codex review (issuecomment-7134020883) and the Astra review (issuecomment-7134187185).
The second blocker below comes from the Astra review.

| Item | Finding | Change | Verify |
|---|---|---|---|
| P90-1 | Codex blocker / Astra blocker 1: password released to a repointed destination | D8, D10, D11, D13 binding, publication rule, 409 codes, `/secrets` removal (D9) | zero-auth tests T1-T6 |
| P90-2 | Astra blocker 2: missing scan tests | zero authentication during scan; scan deadline and socket close against a silent TCP peer | T7, T8 in `flowrun_sftp_test.go` and `runner.test.ts` |
| P90-3 | Codex major: DNS holds slots | deadline before preflight; `dns.promises.Resolver` (c-ares) with `timeout`/`tries` and `cancel()` on the run's abort signal (or a separately bounded pool); admission before graph validation (Astra major `runner.ts:87`) | T9 stalled resolver on both routes |
| P90-4 | Codex major + Astra majors `ConnectionsPanel.tsx:77/83`: edit discards settings / non-atomic save | single credential-bearing PUT; preserve untouched settings and refs; failed save keeps prior pair | T10, canvas test with an API-created connection |
| P90-5 | D1: global cap rejected | remove the global 1 MiB limit at `api.ts`; route-aware 413 for `interactive-run`, `scan`, `secret-bearing` only, decided from method and path before reading | T11, T12 |
| P90-6 | D12 | vetted-IP pinning for `fingerprint` connections and scans; `known_hosts` handling per D12 | T13 |
| P90-7 | Astra majors: Stop status, late workspace response, logout revocation | as listed in the Astra review | existing #90 test plan |
| P90-8 | Codex nit / Astra minor: preview decoding | fatal streaming UTF-8 on the truncated prefix | malformed large payload; code point split at boundary |
| P90-9 | D5 (if approved) | child flags; otherwise canvas Gaps #7 stays documented | child stops at the limit |
| P90-10 | spec amendment | update canvas Run spec D2 scope, Phase 2, Contracts, Compatibility, Gaps #3/#7/#9 and changelog | `node scripts/check-docs-links.mjs` |

### Phase L1a: admission (separate issue, independent of #90)

Auth before body, route-inventory profiles plus coverage test against OpenAPI, unknown routes not
read, D3 bounded parser, explicit `headersTimeout`/`requestTimeout`/`maxHeaderSize`, per-peer pre-auth
in-flight limit (D7), D14 reservation, D4 configuration surface, metrics. No contract change beyond
D3/D14.

### Phase L1b: list streaming (separate issue)

Cursor or bounded iterator on `PgRepository` and `MemoryRepository`, byte-identical envelope,
backpressure, abort on mid-stream failure.

### Later (separate specs)

- L2 revision storage redesign (D6), gated by metrics and a digest parity corpus.
- Runtime listener per-trigger body limit plus `runtime.yaml` ceiling (V10).
- `known_hosts` dial-address mechanism (D12).
- HA: cross-replica quotas, shared secret backend (D7).

## Acceptance criteria

- WHEN an unauthenticated client sends any body to any non-`preauth` route THEN the server answers
  401/404/405 without reading the body, and process RSS does not grow with body size.
- WHEN a body exceeds the `preauth` limit THEN 413 before parsing (if D3 approved).
- WHEN an existing client posts and fetches a revision above 1 MiB (fixed length and chunked, both
  digest modes, memory and PostgreSQL) THEN it gets 201 / 200 with full content and the same digest
  as on `main`.
- WHEN the list holds several revisions whose total exceeds the threshold THEN the response is
  byte-identical in meaning to `main` and is never truncated or omitted.
- WHEN no large-body token is free before the deadline THEN 503 `capacity_unavailable` with
  `Retry-After`; the token is released exactly once.
- WHEN `POST …/test-runs` exceeds 1 MiB THEN 413; other D2 limits behave as in the canvas Run spec.
- **WHEN a credential bound to destination A is used with destination B, a different port, user,
  auth mode, host key or trust set, another connection, another field or another workspace THEN an
  unauthorized peer sees zero authentication attempts** (no TCP connection to it from a run that
  would reveal the credential), and the run returns 409 `credential_unbound` or the PUT returns 409
  `credential_rebind_required`.
- WHEN `secret://` appears outside a credential slot THEN save returns 400 `secret_ref_not_allowed`,
  and a hand-crafted stored row causes no child to start.
- WHEN a save fails (validation, conflict, database error) THEN the prior settings and credential
  still run.
- WHEN a connection is deleted and recreated with the same id THEN the old credential is not usable.
- WHEN DNS stalls THEN the run/scan ends at its deadline and frees its slot on both routes.
- WHEN a scan runs THEN the peer sees zero authentication attempts, and a silent peer is closed at the
  deadline.
- WHEN a run races an edit THEN it either uses the old consistent generation or the new one, never a
  mix, and never reveals a credential whose digest does not match the logical identity of the
  snapshot from which the child's connection file is written.

## Testing

All tests are new. Commands: `pnpm --filter @nurtuo/control-plane run build && pnpm --filter
@nurtuo/control-plane run test`; `cd core/runtime && GOWORK=off go test -race ./cmd/nurtuo/...`;
`pnpm --filter @nurtuo/web run test`. Tests marked SFTP use the pinned CI SFTP fixture.

| # | Test | Where | Notes |
|---|---|---|---|
| T1 | repoint host keeps password -> 409, fake SSH server at B counts 0 auth attempts | `core/control-plane/src/testrun/runner.test.ts` | SFTP; fake peer counts `USERAUTH_REQUEST` |
| T2 | same for port, user, auth mode, host key, `knownHosts` text | same | table-driven |
| T3 | credential of connection A referenced by connection B / another field / another workspace -> rejected, 0 auth at B | `connections/validate.test.ts`, `runner.test.ts` | |
| T4 | `user: "secret://…"` -> 400 on save; stored row -> no child spawned | same | |
| T5 | delete/recreate same id -> old credential unusable | `runner.test.ts` | |
| T6 | concurrent same-binding rotations, multi-field update, run racing edit, failed Pg write, restart (memory loses values -> `missing`) | `connections/*.test.ts` | memory and Pg |
| T7 | scan: 0 auth attempts | `flowrun_sftp_test.go`, `runner.test.ts` | SFTP |
| T8 | scan against silent TCP peer closes at deadline | same | |
| T9 | stalled resolver on run and scan frees slot at deadline | `runner.test.ts` | injected resolver |
| T10 | edit with blank password keeps refs, `timeoutMs`, `maxListEntries` | `core/web` and `connections/routes.test.ts` | API-created connection |
| T11 | revision POST/GET/list above 1 MiB, chunked and fixed, both digest modes, both repositories | `api.test.ts`, `pg-*.test.ts` | Pg tests need a database |
| T12 | test-runs body > 1 MiB -> 413; scan > 4 KiB -> 413 | `api.test.ts` | |
| T13 | fingerprint connection: child connection file holds the vetted IP, binding uses the logical host | `runner.test.ts` | |
| T14 | unauthenticated large body on any route is not read (byte counter on the socket) | `api.test.ts` | L1a |
| T15 | every OpenAPI operation has a limit profile | `limits.test.ts` (new) | L1a |
| T16 | list streaming aborts on mid-stream failure, never completes partially | `api.test.ts` | L1b |

## Gaps and risks

| # | Scenario | Severity | Area | Mitigation |
|---|---|---|---|---|
| 1 | Before L1a, an unauthenticated client streams 2 GiB to any path and the control plane (and its gateway) is OOM-killed | high | availability | L1a first; independent of #90 |
| 2 | With no default ceiling (D2), one authorized client posts a revision that exhausts the heap | medium | availability | D14 token limits concurrent huge trees, but one token alone does not bound process memory (Astra R3): also cap aggregate small-buffer and queue occupancy, isolate materialization above measured headroom; operator ceiling; L2 |
| 3 | Two large uploads at once: the second gets 503 where it succeeded before | low | compatibility | D14 approval, `Retry-After`, threshold from benchmarks |
| 4 | Host key rotates; every run fails until someone who knows the password re-enters it | low | UX | accepted cost of binding trust material; UI explains |
| 5 | `known_hosts` connection: child re-resolves a rebinding name to `127.0.0.1` | medium | security | D12: dial address or network enforcement; recommended default disables the mode on shared installations until then |
| 6 | Go child materializes a 4 GiB payload before the stdout cap | medium | reliability | D5 flags; otherwise documented residual risk |
| 7 | Second control-plane replica uses a same-named local secret | medium | security | single owner in M1 (agreed item 11); HA spec |
| 8 | Initial thresholds (8 MiB, 1 token, envelope size) are wrong for real workloads | medium | performance | benchmark-gated; metrics from L1a |
| 9 | c-ares resolver ignores `/etc/hosts`/search domains, so a host that resolves for the system resolver fails in Run | low | UX | documented; CI uses IP literals with `NURTUO_TEST_RUN_ALLOW_PRIVATE=1` |

## Council

Participants: GPT-6.2 Astra (OpenAI) and Claude Opus (Anthropic). GrokBot moderated. Statements:
Round 1 Astra, Round 2 Opus, Round 3 Astra. They are attached as comments to this spec's pull
request and are not committed (the council brief excludes `council/` from the repository). Opus
Round 2 and Astra Round 3 both conclude that no further round is needed. Astra R3: "The remaining
choices require Łukasz's approval or measurements. Another exchange of expert preferences would
supply neither."

The moderator confirmed that the Round-3 statement is Astra's (GPT-6.2 Astra, run 2026-11-24
17:20-17:22 Europe/Warsaw). The cross-provider (OpenAI) review of this spec is still pending until
the OpenAI usage limit resets (about 19:13 Europe/Warsaw on 2026-11-24).

Outcome: **agreed** = both experts hold the same position; **agreed with qualification** = Astra
R3 accepts Opus's position with conditions, which this spec adopts; **divergent** = positions
differ, owner decision.

| V | Topic | Astra position (R1 → R3) | Opus position (R2) | Outcome | Recommended default |
|---|---|---|---|---|---|
| V1 | First deliverable | R1: admission and Run controls first, storage redesign left open. R3 (modified): ship L1 first; authenticate before collecting protected bodies, isolate expensive operations, meter actual bytes; preserve stable revision sizes; include list streaming when the repository can supply rows incrementally; #90's security fixes proceed independently after approval | L1 (auth before body) first, independent of #90 | agreed with qualification | P90 and L1a in parallel after approval; L1b list streaming as a separate phase (D1, D7) |
| V2 | Revision ceiling | R1: "no new default byte cutoff" on existing endpoints, operator may opt in to a restrictive profile. R3 (disagree): reject a default 256 MiB ceiling on revision POST, GET or list. Creation, storage, deployment and aggregate readback use different representations. Whitespace disappears in `JSON.parse`, so the wire body can exceed the stored content with the same digest; no packing happens on POST; individually modest revisions can make a list above 256 MiB; a 413 on a bodyless GET misdescribes the failure; the owner requires compatibility with existing large uploads. A finite ceiling as an upgrade default needs a separate compatibility decision | R2: explicit 256 MiB 413 on POST, derived from `MaxBundleSize`, `jsonb` and V8. Drafter revision (this spec, not a council round): the whitespace case is checkable on `main`, so a 256 MiB cap would shrink the contract; the drafter withdraws the default ceiling | **divergent** in the council record (R2 vs R3); the drafter's revision aligns with Astra | No default ceiling; operator-configurable; D14 for memory (D2) |
| V3 | Config surface | R1: versioned settings, profile defaults, per-operation overrides, rate and byte budgets, shared quotas for replicas. R3 (modified): operation-specific code defaults and one startup-validated class-level override surface for M1; keep instance budgets and D2 workspace reservations; allow explicit operator restrictions and evidence-backed tuning within physical capacity; "a tighten-only rule relative to an invented revision ceiling has no basis"; defer distributed quotas and general byte-rate controls | code defaults per operation, class-level overrides only, tighten-only except a bounded revision raise, no byte or workspace quotas in M1 | agreed with qualification on the surface; **divergent** on tighten-only | Class-level surface; raise allowed when evidence-backed, within physical capacity (D4) |
| V4 | Status codes | R1: 413 size, 429 quota, capacity as a distinct transient failure. R3 (agree): 413 applicable size limit, 429 caller quota, 503 transient instance capacity, `Retry-After` on throttling or capacity exhaustion; the approved Run/scan concurrency stays 429 on its shared limiter, a 503 there would need an explicit amendment | 413 / 429 / 503 + `Retry-After` | agreed | 413 / 429 / 503; D2 stays 429 (D14) |
| V5 | Credential write API | R1: dedicated atomic credential-save operation, or a staged two-call handle. R3 (agree): optional write-only credentials on the pending connection PUT, retire the pending free-standing secrets API; revise approved D3/D6, OpenAPI, generated client and UI tests together | optional write-only `credentials` on the pending connection PUT; remove `/secrets` | agreed | PUT extension (D8, D9) |
| V6 | Binding representation | R1: immutable grant per credential version, staged versions, conditional generation publish, serialized publish and acquisition. R3 (modified): store the binding digest with each immutable credential version; checking and acquiring that exact version is one store operation; publish settings and version references consistently across concurrent saves, runs and deletes; no lock across network execution. "A digest comparison alone cannot make a database write and an in-memory replacement atomic" | binding digest stored with the credential, checked at reveal, no cross-path lock | agreed with qualification | Digest **plus** the publication rule (Design, D8) |
| V7 | Binding change without re-entry | R1: reject, or save an uncredentialed connection that cannot run. R3 (agree): reject with 409 `credential_rebind_required` before mutation; prompt for fresh values; preserve the working connection on failure | reject with 409 before mutation | agreed | Reject (D8) |
| V8 | `rootPath` | R1: bind it if the product promises subtree confinement. R3 (agree): exclude from the M1 binding; it scopes flow paths on an already authorized account; document that editors can change it; a future subtree promise needs a separate decision | do not bind; document as non-authorizing | agreed | Do not bind (D11) |
| V9 | DNS rebinding | R1: enforce at the actual dial or by network isolation. R3 (modified): pin a vetted dial address for fingerprint mode and scans; keep the logical SSH identity for known_hosts; enforce egress at every actual dial; approve a separate dial-address mechanism or equivalent network enforcement **before enabling that mode on a shared installation**; "best-effort preflight leaves the policy unenforceable" | pin the IP for `fingerprint` in #90; `known_hosts` best effort until a dial flag is approved | agreed on pinning; **divergent** on `known_hosts` meanwhile | Pin now; `known_hosts` disabled on shared installations until a mechanism is approved (D12) |
| V10 | Webhooks | R1: the receiving listener owns size and time policy. R3 (agree): keep production webhook policy with the runtime listener; commission a separate flow-format and runtime-configuration spec with per-trigger overrides and an instance ceiling | same; the knob does not exist yet (runtime fixed 1 MiB) | agreed | Separate runtime spec (non-goal here) |

## Open questions

1. Final D3 value for login after checking field sizes, encodings and existing clients.
2. Benchmarks for the D14 threshold and token count, scan size, parse expansion and the
   `secret-bearing` envelope, on both repositories. No production size distribution, PostgreSQL
   boundary measurement or load-test result has been supplied to the council (Astra R3).
3. Does any instance hold trial data in the #90 connection/secret tables that must be migrated, or
   is re-entry acceptable (assumed acceptable; absent or unbound volatile credentials are treated as
   `missing`)?
4. Name of the D4 configuration surface.
5. Is c-ares DNS (no `/etc/hosts`, different search resolution) acceptable for Run, or is a bounded
   threadpool resolver required?
6. Cross-provider (OpenAI) review of this spec, after the OpenAI usage limit resets.

## Improvement opportunities

- Additive `?content=false` on the revision list (ADDITIVE-ONLY), as a separate decision.
- `If-Match` on connection PUT for lost-update protection in the UI.
- Resumable chunked upload for bulk revisions, if clients need retries.

## References

- Council statements: Astra Round 1, Opus Round 2, Astra Round 3 (PR comments on this spec's pull
  request).
- Codex review and Astra review of PR #90, owner decision comment (links in Status).

## Changelog

- 2026-11-24: created as a council draft by Claude Opus. Status draft, needs owner decision.
- 2026-11-24: Astra's Round 3 (confirmed by the moderator) recorded. Council table now gives each
  V-item's outcome; D-list marks D2, D4 (override rule) and D12 (`known_hosts`) as divergent. Binding
  check now uses the snapshot's logical identity, not the child file after IP substitution. Status
  still draft, needs owner decision.
