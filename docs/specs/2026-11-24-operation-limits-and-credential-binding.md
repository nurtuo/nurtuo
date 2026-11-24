<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Operation-aware limits and stored-credential binding

Status: draft (needs owner decision). Date: 2026-11-24.

## TLDR

Replace the global 1 MiB request cap from the canvas Run spec with limits chosen per operation, and
check that control-plane SFTP credentials are bound to their authorized destination before they are
released. The first protection step is to authenticate before reading a body. The global cap is
not that step. Existing revision uploads keep working: no new default 413 on revision routes.
Memory is bounded by a byte-weighted reservation taken **before** a body, row or list is
materialized, with isolation for the largest operations. Credential release is a server-side
authorization decision. The decisive test is that **an unauthorized peer sees zero authentication
attempts**, measured at the socket.

This spec is the written outcome of a two-expert council (Claude Opus and GPT-6.2 Astra). The
council ran three rounds (Astra R1, Opus R2, Astra R3) and both experts say no further round is
needed. Three points stay divergent and are owner decisions: the revision ceiling (D2), the
override rule (D4) and `known_hosts` dialing (D12). A cross-provider review by Codex (gpt-6.2-sol,
OpenAI) requested changes; this revision answers it (see "Cross-provider review"). Łukasz must decide
D1-D14 before any code is written, including the PR #90 rework.

## Status and links

| | |
|---|---|
| Pull requests | this spec (draft). Affects [PR #90](https://github.com/nurtuo/nurtuo/pull/90) (canvas Run implementation, head `aa02133`) |
| Reviews it answers | Codex review https://github.com/nurtuo/nurtuo/pull/90#issuecomment-7134020883, Astra review https://github.com/nurtuo/nurtuo/pull/90#issuecomment-7134187185, owner decision https://github.com/nurtuo/nurtuo/pull/90#issuecomment-7134459612, cross-provider review of this spec https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7140661227 |
| Amends | [canvas Run](2026-11-23-canvas-run.md): D2 scope, D4 (`known_hosts` paste), D11 egress (check at the actual dial), Phase 2 body limit, D3/D6 secret workflow, Gaps #3, #7, #9, #19; [backward compatibility contract](2026-11-22-backward-compatibility-contract.md): classification rows for the D4 surface and `nurtuo flow` (D4) |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md) (connections and secrets), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (gateway), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) (engine) |
| Related specs | [backward compatibility contract](2026-11-22-backward-compatibility-contract.md), [auth rate limiting and lockout](2026-11-22-auth-rate-limiting-and-lockout.md), [bundle artifact delivery](2026-11-22-bundle-artifact-delivery.md), [HTTP listener routing](2026-11-22-http-listener-routing.md) |
| Code (main `fbdb9e8`) | `core/control-plane/src/api.ts` (`createHttpServer`, `canonicalJson`), `core/control-plane/src/auth/policy.ts` (`POLICIES`), `core/control-plane/src/auth/throttle.ts`, `core/control-plane/src/auth/middleware.ts` (`NURTUO_TRUST_PROXY`), `core/control-plane/src/pg-repository.ts`, `core/control-plane/src/memory-repository.ts`, `core/runtime/internal/listener/http.go` (`MaxBodyBytes`), `core/runtime/internal/connections/connections.go` (`Resolver`), `connectors/sftp/sftp.go` (`dial`), `connectors/sftp/config.go` (`sshConfig`) |
| Code (PR #90 `aa02133`) | `core/control-plane/src/testrun/runner.ts`, `core/control-plane/src/testrun/egress.ts`, `core/control-plane/src/connections/routes.ts`, `core/control-plane/src/connections/validate.ts`, `core/web/src/ui/ConnectionsPanel.tsx` |

## Owner requirement

Łukasz, 2026-11-24 15:29 Europe/Warsaw, recorded on PR #90 (verbatim):

> Nurtuo jest platformą integracji systemów, która musi obsługiwać duże payloady, rewizje, webhooki oraz masową wymianę danych. Zachowaj kompatybilność istniejącego API. Zaprojektuj konfigurowalne limity zależne od rodzaju endpointu i charakteru operacji. Uwzględnij streaming, ochronę pamięci, rate limiting oraz bezpieczeństwo. Nie wprowadzaj arbitralnych limitów bez analizy rzeczywistych wymagań platformy. Zleć Opusowi i Astrze uzgodnienie rozwiązania architektonicznego przed implementacją.

He rejected a global 1 MiB limit (PR #90 comment 7134459612: "no new 413s for current large revision
uploads"). Where a review request conflicts with this decision, this spec follows Łukasz and says so
(the cases are marked "owner decision prevails").

## Problem and goals

Facts on `main` (`fbdb9e8`):

1. `createHttpServer` (`core/control-plane/src/api.ts`) collects every chunk, decodes UTF-8 and runs
   `JSON.parse` before it calls `handle()`. Authentication runs inside `handle()`. An
   unauthenticated client can therefore send a body of any size, on any path including unknown
   ones, for up to Node's default `requestTimeout` (300 s), and the process holds all of it. The
   same process hosts the runtime gateway (`main.ts`). This is the largest memory exposure today.
   Because parsing comes first, a malformed body gets 400 `invalid JSON` on every path, before 401,
   403, 404 or 405.
2. Implicit ceilings already exist and fail badly. A string above `buffer.constants.MAX_STRING_LENGTH`
   (536,870,888 UTF-16 units on this tree's Node) makes decoding throw inside the parse `try`, so a
   well-formed document gets 400 `invalid JSON`. Heap exhaustion usually comes earlier and kills the
   process. PostgreSQL `jsonb` has its own internal-size limit, which turns into a 500. That limit is
   not a wire-size bound (see V2).
3. `GET /v1/components/{id}/revisions` returns every revision with full content
   (`pg-repository.ts`, `SELECT *`). It is serialized with one `JSON.stringify`. `MemoryRepository`
   retains every revision object for the life of the process.
4. The runtime webhook listener streams through `http.MaxBytesReader` with a fixed 1 MiB default
   (`listener/http.go`, `MaxBodyBytes`). The factory does not set it and `nurtuo.flow/v1` has no field
   for it.
5. The runtime `connections.Resolver` replaces `secret://` in any string setting, not only in
   credential fields.
6. The SFTP connector dials `cfg.addr()` and passes the same address to `ssh.NewClientConn`
   (`connectors/sftp/sftp.go`), so the dial address and the host name used for `known_hosts`
   verification cannot differ today.
7. The control-plane listener is plain HTTP. Login performs no server-side TLS check; the session
   cookie is `Secure`, so browsers send it only over HTTPS or to loopback.

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
    which get a bounded parser. Working-set memory is reserved before a body, row or list is
    materialized. One large operation cannot take down the co-hosted gateway.
  - Rate limiting: per-peer and instance-wide pre-auth admission, login throttle (kept), D2 run
    concurrency, and targeted local rate budgets where concurrency does not bound the work (D7).
  - Security: a stored credential reaches only the destination and trust configuration it was
    entered for. `secret://` appears only in credential slots. Egress is enforced at the actual dial;
    where it cannot be, execution fails closed.
  - Default values come from platform constants, a stated formula or the approved D2 analysis,
    never from a percentile guess. Values without a basis are marked "initial, benchmark-gated".
- Non-goals:
  - Streaming revision storage redesign (L2). It gets its own spec and is triggered by metrics (D6).
  - Runtime webhook listener limits. A separate follow-up spec covers them; its required scope is
    listed under "Later" (V10, D7).
  - Cross-replica quotas, byte-rate quotas, shared quota service. These belong to a later HA spec (D7).
  - A persistent encrypted secret store (canvas Run Gaps #4, #5).
  - A TLS listener in the control plane. TLS is established by a trusted proxy or by loopback (D8).
  - Mutating SFTP operations, other connectors in Run (canvas D11 unchanged except as amended).
  - Reopening canvas Run decisions other than those named under "Amends".

## Decisions (pending)

Nothing below is approved. Each item names the recommended default. Letters in brackets map to the
council ledgers (`D-Lx` limits, `D-Sx` secrets). The "Council" column gives the outcome after
Round 3: **agreed**, **agreed with qualification** (the qualification is part of the recommended
default), or **divergent** (both positions given; Łukasz decides). See the Council section for each
V-item. Changes made after the cross-provider review are part of the recommended default and are
listed under "Cross-provider review"; they do not change any council outcome.

| # | Question | Council | Recommended default | Alternative |
|---|---|---|---|---|
| D1 [D-L1] | Global 1 MiB cap | agreed (V1) | Supersede the global Phase 2 cap and its test. D2's values (1 MiB input, 20 documents and 1 MiB total decoded output, 30/120 s, 2/workspace, 4/instance shared slots) apply to `POST …/test-runs` only. Scan has its own small profile (body size from the formula in "Operation classes", 10 s, shares the run limiter) | keep global cap (rejected by owner) |
| D2 [D-L2] | Default body ceiling on revision routes | **divergent** (V2) | **No default byte ceiling** on `POST /v1/components/{id}/revisions`. Never a 413 on GET or list. Revision upload and full-content readback are preserved by default. An operator may configure a documented finite ingress ceiling. Making one an upgrade default would need a separate compatibility decision. Memory is bounded by the L1a memory budget (D14), which reserves before materializing and isolates the largest operations; it never rejects a request for its own size alone (owner decision prevails over a size cutoff). Position of Astra (R1, R3), adopted by the drafter | explicit 256 MiB 413 on revision POST (Opus R2, which also named the matching response paths; the drafter withdraws it, see V2) |
| D3 [D-L3] | Bounded parser on body-authenticated routes | agreed (V1) | Approve in principle a bounded, memory-only parser for `POST /v1/auth/login` (and any other route whose authentication needs body fields). 64 KiB is a candidate, final value after field/encoding/client evidence. No blanket cap on public routes. **Rollout: the pre-1.0 deprecation protocol** (spec, OpenAPI `413` on login, `docs/upgrade.md` entry, bridge "documented in the same release"). The emergency security exception is not used unless the SECURITY.md process rates Gaps #1 high or critical and issues an advisory | no change (unauthenticated body stays unbounded on login) |
| D4 [D-L4] | Configuration surface | agreed with qualification; **divergent** on the override rule (V3) | Operation-specific defaults in code, keyed by policy action, method/template coverage tested against OpenAPI, server-selected profiles enforced. Operator overrides **per class** via one surface (name chosen by Łukasz, e.g. `NURTUO_LIMITS_FILE`), with units, scope and defaults documented and validated at startup against a resource budget. Overrides may tighten any dimension (explicit operator restrictions) and may raise only the dimensions listed as tunable, when evidence supports it, within the instance's physical capacity (Astra R3); safety ceilings (`preauth`, `secret-bearing`, `scan` bodies) are raised only by a code change. **The entire connection PUT** uses the bounded, memory-only `secret-bearing` profile. Classification: the surface and its variable are **EXPERIMENTAL**, recorded in the compatibility contract's table (amendment) | tighten-only, except a bounded raise of the revision ceiling (Opus R2) |
| D5 [D-L5] | Child output bound | agreed with qualification (Astra ledger) | Add `nurtuo flow run --max-output-bytes` and `--max-documents`, applied in the child before a whole payload is read or a result set accumulated, and counting metadata, encoded output and temporary disk. Overflow keeps the D2 failed result (`output_too_large`) with no documents. **Run is enabled only with these producer bounds or equivalent tested isolation.** Because Run is never served in auth-disabled mode (canvas D9), every instance that serves Run counts as shared, so the condition applies everywhere: P90-9 is required for #90, not optional. The flags are EXPERIMENTAL like `nurtuo flow` (compatibility table amendment, D4) | ship #90 with the residual risk documented (canvas Gaps #7); not recommended |
| D6 [D-L6] | Revision storage redesign (L2) | agreed with qualification (Astra ledger) | Separate spec, triggered by measured demand or by failure to meet the working-set budget. Before switching it must prove digest parity against `canonicalJson` (duplicate and prototype-named keys, UTF-16 ordering, UTF-8 replacements, lone surrogates, negative zero, overflowing numbers), bounded sorting, old-row readback, atomic publication and cleanup | include in first delivery |
| D7 [D-L7] | Rate limiting and quotas in M1 | agreed with qualification (V3, Astra ledger) | Instance-local admission and one volatile-secret owner. Keep login throttle/lockout. Add a bounded per-peer pre-auth in-flight limit (trusted-proxy identity, bounded key cardinality), an **instance-wide ingress budget** (requests holding a body or reservation, bounded wait queue), and **targeted local rate budgets** for work that concurrency does not bound: failed authentications per peer and scan/run dials per workspace and instance. Keep D2 concurrency. Reserve capacity for reads, writes and teardown, and test recovery after disconnects and timeouts. Metrics by operation (bytes, rates, CPU, RSS, spool, wait, rejection) with no bodies or identifiers. No byte-rate or workspace quotas; a later HA spec owns cross-replica fairness. Webhook bursts belong to the runtime follow-up | add workspace/byte-rate quotas now |
| D8 [D-S1] | Credential write API | agreed (V5, V7); agreed with qualification on the publication rule (V6) | Optional write-only `credentials: {password?, privateKey?, passphrase?}` member on the pending `PUT /v1/workspaces/{ws}/connections/{id}`. Per field: **omitted = keep** (only when the binding is unchanged), **string = new version**, **`null` = remove**; empty string is 400. The effective set must be a valid SFTP combination (see "Credential lifecycle"). A PUT that sets or removes a credential needs `connection:write` plus `secret:write` (set) or `secret:delete` (remove), a user session, CSRF and trusted TLS (proxy-asserted under `NURTUO_TRUST_PROXY=1`, or a loopback peer). An unchanged-binding edit needs `connection:write` only. Hidden settings and server-owned references are preserved; client-selected substitutes are rejected. A binding change with retained credentials returns **409 `credential_rebind_required` before any mutation**. Publication follows the V6 rule. An uncertain commit is reconciled before the outcome is reported | dedicated credential-save operation; staged two-call handle (Astra R1, superseded by Astra R3) |
| D9 [D-S2] | Free-standing `/secrets` routes | agreed (V5) | Remove `GET …/secrets`, `PUT|DELETE …/secrets/{name}` from the pending API. Connections report `secrets: {field: set|missing}`. Intentional removal is `null` in `credentials` on the connection PUT, with `secret:delete`. Deleting a connection (`connection:delete`) or a workspace purges its credentials without a separate secret permission, because the connection owns them | keep them as storage that the runner never releases |
| D10 [D-S3] | `secret://` placement | agreed (Astra ledger) | Allowed only in schema credential slots (`writeOnly` / `x-nurtuo-secret`), server-assigned for this connection and field. Elsewhere 400 `secret_ref_not_allowed` on save, and refusal at snapshot time for a malformed stored row. All connections are validated before any child input is released. Applies to control-plane-managed Run connections only. Standalone providers, bundle syntax and `secret://` stay unchanged | none proposed |
| D11 [D-S4] | `rootPath` in the binding | agreed (V8) | Not bound; editors can change it. Documented as a flow-path convenience on an already authorized account, not an authorization boundary. A future subtree-authorization promise needs a separate decision | bind it |
| D12 [D-S5] | DNS rebinding / dial address | **divergent** on `known_hosts` (V9) | Pin a vetted dial address for `fingerprint` connections and scans, keeping the logical SSH identity for the binding, with the egress check at every actual dial (scan, retries, both address families). **Fail closed wherever actual-dial enforcement is unavailable**: on every installation, a control-plane Run or scan of a `known_hosts` connection is refused before any network contact (409 `dial_enforcement_unavailable`) until an approved dial-address mechanism (connector and child flag, Ask First) verifies the logical host while dialing the pinned address. Canvas D4 is amended: a pasted `known_hosts` entry is converted at save to `hostKeyPolicy: fingerprint` when it has exactly one key for the logical host and port; otherwise 400 asks the user to pick a fingerprint (Astra R3) | `known_hosts` enabled as best effort and documented until a dial flag is approved (Opus R2) |
| D13 [D-S6] | Revocation | agreed (Astra ledger) | Delete or rebind revokes future acquisitions. An admitted run finishes under its D2 deadline plus bounded teardown grace (initial 5 s). Current permission is checked and the full snapshot is acquired at admission, atomically with respect to workspace and connection deletion. UI logout revocation and failure reporting stay on #90 | kill in-flight runs |
| D14 | Memory budget, reservation and capacity error | agreed (V4) | One byte-weighted process memory budget. A body, single row or list is reserved **before it is materialized** (declared length, counted chunks, stored size). Operations above the in-memory threshold (initial 8 MiB, benchmark-gated) also run isolated (worker with heap limits). A reservation larger than the whole large-operation budget is admitted **exclusively**, never rejected for its size. **503 `capacity_unavailable` + `Retry-After`** only when other reservations hold the budget until the wait deadline. Rollout of the new 503 on STABLE routes follows the pre-1.0 deprecation protocol (same as D3). 413 = applicable size limit, 429 = caller quota or rate (D2 `too_many_runs` stays 429), 503 = instance capacity. Release gated on load tests that show the co-hosted gateway survives | single large-body token only (does not bound process memory; Astra R3, cross-provider review) |

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
   contained by a requests-per-second budget. Login throttle and lockout stay. (D7 adds targeted
   rate budgets for the cases that concurrency does not bound.)
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
| `preauth` | `POST /v1/auth/login`, other body-authenticated routes | bounded, memory only, candidate 64 KiB (D3); safety ceiling | login throttle; per-peer in-flight limit; failed-auth rate budget (D7) | largest legitimate input is credentials and tokens |
| `control` | authenticated management JSON (component metadata, runtimes, deployments, members, tokens, workspaces, roles, SCIM) | no new default ceiling; D14 reservation | listener-wide `headersTimeout`, `requestTimeout` (300 s kept), `maxHeaderSize` set explicitly | compatibility of STABLE routes |
| `document` | `POST`/`GET /v1/components/{id}/revisions`, `GET …/revisions/{n}` | no default ceiling (D2); D14 reservation and isolation; operator may set a ceiling | `requestTimeout` 300 s kept | owner requirement; whitespace and aggregate-list cases in V2 |
| `secret-bearing` | **every** `PUT /v1/workspaces/{ws}/connections/{id}`, with or without `credentials` (D4, D8) | bounded, memory only, never staged to disk, traced or hashed; envelope from the formula below; safety ceiling | user session for credential changes (D8) | sensitive values of known size |
| `interactive-run` | `POST /v1/workspaces/{ws}/test-runs` | 1 MiB (D2), 413 | 30 s default, 120 s max; 2/workspace, 4/instance; 429 `too_many_runs`; dial rate budget (D7) | approved D2 |
| `scan` | host-key scan | from the formula below (placeholder 4 KiB); safety ceiling | 10 s; shares the run limiter; dial rate budget (D7) | tiny request that opens a socket |

**Envelope formulas (not percentiles).** The `secret-bearing` envelope is
`Σ escapedMax(MAX_SECRET_BYTES) over password, privateKey, passphrase + escapedMax(knownHosts cap) + fixed settings allowance`,
where `escapedMax(n)` is the largest JSON wire size of a valid value of `n` decoded bytes (`6n` for
control characters escaped as `\uXXXX`, the worst case), `MAX_SECRET_BYTES` is 64 KiB per decoded
value (#90), and the `knownHosts` cap is a new validation limit on the pending connection schema
(candidate 64 KiB). The fixed allowance covers the non-secret SFTP settings at their schema maxima.
The `scan` body is the escaped maximum of its fields (host name 253 octets, port, user, policy) plus
the same allowance. P90 computes both from these formulas in code and ships them without waiting for
L1a: the gate is a test in which the largest valid escaped request succeeds and one more byte gets
413. L1a benchmarks may only tighten them; raising needs a spec change.

The 8 MiB threshold, the budget fractions and the expansion factor below are placeholders. L1a
benchmarks replace them, and each change is recorded in the changelog.

### Memory budget for the first release (L1a, D2, D14)

One large-body token does not bound process memory: a single admitted request still allocates
chunk buffers, the decoded string, the parsed tree, the canonical serialization and repository
copies. L1a therefore uses a **byte-weighted budget**, reserved before allocation:

- **Budget.** `memoryBudget` = V8 `heap_size_limit` minus a fixed headroom for the gateway, the
  event loop and the small-request pool. The headroom and pool sizes are startup-validated (D4).
  `MemoryRepository` retained history (bytes of retained revisions, tracked at creation) is
  subtracted from the budget continuously, because it is resident for the life of the process.
- **Estimate.** A reservation is `k × wireBytes` for a body (decoded string, tree, canonical
  string, repository copy) and `k × storedBytes` for a response. `k` is the measured expansion
  factor per class (placeholder, benchmark-gated with high-expansion JSON: deep nesting, many tiny
  objects, long escaped strings).
- **Fixed-length upload.** With `Content-Length`, the full reservation is taken after
  authorization and **before the first body byte is read**. While waiting the socket is paused.
- **Chunked upload.** Without `Content-Length`, bytes are read into the small-request pool up to
  the in-memory threshold. Each further step (the reservation doubles, starting at the threshold) is
  reserved before more bytes are read; the socket is paused while waiting. A step that cannot be
  reserved before the wait deadline ends the request as below.
- **Single row.** `GET …/revisions/{n}` and every internal read of one revision (deploy, digest
  checks) first read the stored size (PostgreSQL: `octet_length(content::text)` or a size column added
  by migration, measured in L1a; memory: size kept at creation), reserve, then load the row.
- **Revision list.** Before L1b, L1a reads the total stored size of the matching rows with one size
  query, reserves `k × total` and only then loads them. L1b replaces this with an iterator that
  reserves per row (largest row plus write buffers). The list is never truncated or omitted.
- **Responses.** A response reservation is held until the client drains it or disconnects. Slow
  readers keep their reservation; that is why the wait deadline and the instance-wide ingress
  budget (D7) exist.
- **Isolation.** Above the in-memory threshold, decode, parse, `canonicalJson`, digest and response
  serialization run in a worker thread with `resourceLimits` sized to the reservation. A worker
  heap failure ends that worker and that request only (500 `resource_exhausted`), never the process
  or the gateway. On `main` the same request would kill the whole process, so no request that
  succeeds today is lost by this (the load-test gate below proves it). A child process is the
  alternative if measurement shows worker transfer cost is too high; L1a chooses by measurement.
- **Exclusive admission.** A reservation larger than the whole large-operation budget is not
  rejected (owner decision prevails: no new default 413). It waits for exclusive admission: new
  large reservations queue behind it, it starts when the large-operation budget is idle, and it runs
  isolated.
- **When 503 is permitted.** Only when a reservation cannot be obtained before the wait deadline
  because **other** reservations hold the budget (transient). Never because of the request's own
  size. The wait deadline is the remainder of the class `requestTimeout`. A 503 carries
  `Retry-After` and `Connection: close`. Each reservation is released exactly once, including on
  disconnect, abort and worker failure.
- **Load-test gate (blocks L1a release).** On the reference heap: (1) establish `S_main`, the largest
  revision POST, GET and list that succeed on `main` with an otherwise idle instance; the release
  must succeed at `S_main` too, with the same digest. (2) With high-expansion JSON, concurrent large
  and small reads and writes, slow readers, mid-upload and mid-response disconnects, and uploads
  above `S_main`, the process is not killed, the gateway's mTLS listener keeps answering its
  heartbeat within its existing timeout, peak RSS stays within `heap_size_limit + documented native
  overhead`, and all reservations return to zero afterwards.

### Admission, unread bodies and error precedence (L1a, D3, D14)

- **Rejected unread bodies.** A response decided before reading the body (401, 403, 404, 405, 413,
  503) is sent with `Connection: close`; the server stops reading and destroys the socket after the
  response is flushed (bounded grace, initial 1 s). It never drains an unread body. For
  `Expect: 100-continue` the final status is sent instead of `100 Continue`. Application-level body
  consumption is zero; kernel and Node socket buffering is bounded by the socket high-water mark.
- **Error precedence (changed, documented).** Today a malformed body gets 400 `invalid JSON` on every
  path first. After L1a the order is: 404/405 (route), 401 (authentication), 403 (CSRF, permission),
  413 (declared size over a class limit), 503 (capacity), then 400 `invalid JSON` and validation.
  Clients that send malformed bodies to unknown or unauthorized routes see the more specific status.
  This is a change of a STABLE route's error semantics and is part of the D3/D14 upgrade note.
- **Auth-disabled mode.** With `NURTUO_INSECURE_NO_AUTH=1` the core routes keep reading and parsing
  their bodies as on `main` (behind the D14 reservation). Pending workspace routes still answer 404
  without reading the body (canvas D9).
- **SCIM.** SCIM routes have public policy entries; their route-local bearer authorization runs
  before the mutation body is read.

### Rate limiting and ingress budgets (L1a, D7)

Concurrency caps bound memory and stalls; they do not bound CPU or outbound dials from fast,
short, sequential requests. M1 therefore adds two local budgets, all values initial and
benchmark-gated:

- **Instance-wide ingress budget.** Maximum requests that hold an unread or partly read body or a
  D14 reservation (placeholder 256), plus a bounded wait queue (placeholder 64 waiters). When the
  queue is full, new work gets 503 `capacity_unavailable` immediately. Per-peer pre-auth in-flight
  limit (placeholder 8) keyed by the trusted transport identity. The key table is bounded
  (placeholder 10,000 entries); when full, unknown peers share one overflow key, so the table cannot
  grow and the overflow key is limited like a single peer.
- **Targeted rate budgets.** A token bucket for failed authentications per peer (placeholder 10/s,
  burst 20; exceeding it returns the existing 429 `rate_limited` with `Retry-After`; the login
  throttle and lockout stay as they are). A token bucket for scan and run dials per workspace and
  per instance (placeholder 30/min per workspace), because a scan holds a slot for milliseconds and
  the D2 slots alone do not bound dial rate; exceeding it returns 429 `too_many_runs` with
  `Retry-After` (existing D2 code). Successful authenticated requests have no rate budget in M1: their
  cost is bounded by the reservation and the ingress budget, and the metrics show whether that is
  enough.
- **Recovery.** Timeouts, disconnects and aborts release the ingress slot, the per-peer count, the
  reservation and the run slot exactly once. Teardown has its own bounded grace (D13) and is not
  counted against new admissions after it ends.

### Streaming

- **Request bodies.** L1a reads only after authorization and reservation. For `document` and
  `control`, bodies below the threshold stay in memory. Above it they are parsed in isolation as
  above. A true incremental parse of a revision is L2 (D6), because `canonicalJson` must keep its
  exact semantics (UTF-8 replacement before parse, last duplicate key wins, `__proto__` as own key,
  `Number`/`String` round trip, UTF-16 key order, lone surrogates escaped).
- **List response.** Writing the existing array one item at a time removes the one large string.
  The full fix is a cursor or bounded iterator with a consistent read snapshot that keeps the `items`
  envelope, order and full content, honors backpressure and closes on disconnect. A failure after
  headers are sent must abort the stream. A partial list must never complete as a success. Phase
  L1b covers this; until then L1a's size-first reservation applies.
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
  `authMode` is the set of credential fields in the effective combination (see below). Normalization
  covers only what the dialer and verifier both treat as equal: host lower-cased, a trailing dot
  dropped, no DNS resolution, port defaults to 22, user exact, `knownHosts` text as stored. Absolute
  DNS names are normalized only where lookup, dial and verification agree. Any mismatch fails closed.
  The digest hashes only non-secret settings, so it may be audited.
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
  different hosts. On mismatch: 409 `credential_unbound`, no child, no network contact. Prior
  versions stay alive while an acquired snapshot uses them. No lock is held across network execution.
  An optional `If-Match` on a server ETag prevents lost edits but is not needed for correctness:
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

### Credential lifecycle (D8, D9, D13)

- **Per-field semantics** in `credentials`: member omitted = keep the current version (allowed only
  when the binding is unchanged, otherwise 409 `credential_rebind_required`); non-empty string = new
  immutable version; `null` = remove the field; empty string = 400 `invalid_credential`.
- **Valid effective SFTP combinations** (after applying the PUT): `{password}`, `{privateKey}`,
  `{privateKey, passphrase}`, `{privateKey, password}`, `{privateKey, passphrase, password}`. The
  connector offers public key first, then password (`connectors/sftp/config.go`). Invalid: no
  credential, `passphrase` without `privateKey`, a `privateKey` that does not parse (with the
  passphrase if one is set). Parsing happens in the control plane at save, in memory. Invalid
  combination: 400 `invalid_credential_set`, nothing mutated. A change of the combination is a
  binding change (`authMode`), so every remaining field must be re-entered in the same PUT.
- **Authorization.** Setting a value: `connection:write` + `secret:write`. Removing a field:
  `connection:write` + `secret:delete`. Both: all three. Deleting the connection: `connection:delete`
  only; deleting the workspace: the existing workspace-delete permission. Deletion purges every
  version after in-flight acquisitions end (D13). Credential changes are `userOnly` (no API tokens,
  canvas D6) and need the CSRF header. The body is parsed (bounded, D4) before the credential checks,
  and every check runs before any mutation.
- **Trusted TLS.** The listener is plain HTTP (fact 7), so TLS is established by a proxy. A
  credential-bearing PUT is accepted only if (a) `NURTUO_TRUST_PROXY=1` and the immediate peer's
  `X-Forwarded-Proto` is `https`, or (b) `NURTUO_TRUST_PROXY` is unset and the immediate peer is a
  loopback address (local development and the Vite proxy). Otherwise 403 `tls_required`, nothing
  mutated. An operator with a non-loopback TLS-terminating proxy must set `NURTUO_TRUST_PROXY=1`,
  which is already required for correct client addresses. No new configuration key.
- **Uncertain commits.** Every publication writes a server-generated generation id. If the database
  outcome is uncertain (connection lost after `COMMIT` was sent, timeout), the server reads the
  committed generation **before reporting an outcome**: committed means 200, not committed means the
  error and the prior state, unchanged. If the generation cannot be read before the request
  deadline, the response is 503 `save_outcome_unknown` (never a claimed failure or success), staged
  versions are kept, and a reconciler resolves them on the next store access or at startup. Cleanup
  of staged versions happens only after reconciliation.
- **Restart.** PostgreSQL connection rows survive a restart; volatile credential versions do not.
  After restart each field reports `missing`, a run gets the existing #90 missing-credential error
  with no child and no network contact, and re-entry (a credential-bearing PUT) restores it.
- **Deletion races.** Admission checks current permission and acquires the full snapshot in one store
  operation that is serialized with connection and workspace deletion. A run that acquired first
  finishes under D13; a deletion that won first makes the run 404 with no reveal.

### Dial-address enforcement and `known_hosts` (D12)

- **Enforcement rule.** A control-plane Run or scan opens a network connection only to an address
  that passed the egress check (canvas D11) and that is the address actually dialed. The parent
  resolves the logical host with the cancellable resolver (P90-3), refuses the run if **any** A or
  AAAA answer is disallowed, chooses one allowed address, and passes it to the child. Every dial of
  the run (connection test in `Prepare`, the operation, any retry) uses that address. No address is
  ever re-resolved by the child.
- **`fingerprint` mode.** The child connection file holds the vetted address as `host`. The binding
  digest and audit use the logical host. Fingerprint verification does not depend on the name.
- **`known_hosts` mode fails closed.** The connector dials and verifies with the same address
  (fact 6), so a pinned address would break host-name matching, and an unpinned dial cannot be
  enforced. Until an approved mechanism (connector setting for a dial address plus a `nurtuo flow`
  flag, Ask First) verifies the logical host while dialing the pinned address, a Run or scan of a
  stored `known_hosts` connection is refused before any network contact with 409
  `dial_enforcement_unavailable`, on every installation. "Equivalent network enforcement" (an egress
  proxy or network policy) would need its own approved decision and test before it can lift this.
- **API and UI (amends canvas D4).** The API still accepts pasted `known_hosts` content as input to
  the trust step. At save it converts the content to `hostKeyPolicy: fingerprint` when the content
  has exactly one key entry for the logical host and port. With zero or several matching keys it
  returns 400 `known_hosts_ambiguous` and the UI asks the user to choose a fingerprint (the TOFU
  scan or a pasted fingerprint). `hostKeyPolicy: known_hosts` sent directly is 400
  `host_key_policy_not_supported` on the control-plane connection API. The UI shows no `known_hosts`
  mode. Standalone and bundle connections are unchanged.
- **Egress contract amendments.** Canvas D11: the egress check applies at the actual dial (all
  dials of a run and scan, both address families), not only in preflight. Canvas Gaps #3 becomes
  "closed for `fingerprint`; `known_hosts` refused"; Gaps #19 is resolved by this rule.

### Failure modes

| Case | Response |
|---|---|
| body over a class limit (`interactive-run`, `scan`, `preauth`, `secret-bearing`, operator-set ceiling) | 413 `payload_too_large`, before parsing (declared) or when the count crosses it (chunked) |
| unknown route | 404/405 as today, body not read, `Connection: close` |
| unauthenticated or unauthorized request with a body | 401/403 as today, body not read, `Connection: close` |
| reservation not available before the wait deadline, or ingress queue full | 503 `capacity_unavailable`, `Retry-After` (D14, D7) |
| isolated worker exceeds its heap | 500 `resource_exhausted`, process and gateway unaffected (D14) |
| run/scan concurrency or dial rate exhausted | 429 `too_many_runs`, `Retry-After` (D2, D7) |
| failed-authentication rate exceeded | 429 `rate_limited`, `Retry-After` (existing code) |
| binding changed without fresh credentials on PUT | 409 `credential_rebind_required`, nothing mutated |
| invalid credential combination or empty value | 400 `invalid_credential_set` / `invalid_credential`, nothing mutated |
| credential PUT without trusted TLS | 403 `tls_required`, nothing mutated |
| save outcome cannot be determined before the deadline | 503 `save_outcome_unknown`, reconciled later |
| stored binding does not match run snapshot | 409 `credential_unbound`, no child, no network contact |
| run or scan of a stored `known_hosts` connection | 409 `dial_enforcement_unavailable`, no network contact |
| `secret://` outside a credential slot | 400 `secret_ref_not_allowed` on save; run refused for a stored row |
| DNS stalls past the deadline | run `failed` with timeout, slot released at the deadline |
| child output over `--max-output-bytes` / `--max-documents` | run `failed` `output_too_large`, no documents, temp dir removed, child reaped |

## Contracts

| Contract | Change | Stability |
|---|---|---|
| STABLE control-plane routes | body read after auth; changed error precedence; no new default 413; possible 503 `capacity_unavailable` and 500 `resource_exhausted` (D14); login bounded (D3) | STABLE; rollout by the pre-1.0 deprecation protocol (D3, D14 need approval) |
| `POST …/test-runs`, host-key scan | D2 values scoped to these routes; dial rate budget; `dial_enforcement_unavailable` | experimental (pending, #90) |
| `PUT …/connections/{id}` | bounded `secret-bearing` profile for the whole route; optional write-only `credentials` member with keep/set/remove; 409 `credential_rebind_required`; 400 `invalid_credential_set`, `known_hosts_ambiguous`, `host_key_policy_not_supported`; 403 `tls_required`; 503 `save_outcome_unknown`; read-only server-assigned refs (D8, D12) | experimental (pending, #90) |
| `…/secrets` routes | removed from pending API (D9) | experimental (pending, #90) |
| run error `credential_unbound` (409), save error `secret_ref_not_allowed` (400) | new | experimental |
| limits configuration surface and its variable (D4) | new, name chosen by owner; added to the compatibility contract's classification table | EXPERIMENTAL (until a later decision makes it STABLE) |
| `nurtuo flow run --max-output-bytes`, `--max-documents` (D5) | new, additive | EXPERIMENTAL; the compatibility table gains the row "`nurtuo flow` (subcommand and flags): EXPERIMENTAL", matching `docs/cli.md` ("The contract is experimental") |
| `secret://` syntax, `nurtuo.connection/v1`, bundle syntax | unchanged | frozen / stable |

The compatibility contract lists control-plane environment variables and `nurtuo` flags as STABLE,
with named EXPERIMENTAL exceptions. D4 and D5 add explicit exceptions there in the same PR as the
code; that is a public contract change and is covered by Łukasz's approval of D4 and D5.

## Compatibility

- Revision create, get and list keep today's accepted sizes and full content (D2). The canvas Run
  spec's test "body over 1 MiB is 413" on all routes is replaced by per-route tests.
- **One rollout path for D3 and D14**: the pre-1.0 deprecation protocol of the compatibility
  contract, with the bridge "documented in the same release". The release that ships L1a adds to
  `openapi.yaml`: `413` on `POST /v1/auth/login`, `503` with `Retry-After` and `500
  resource_exhausted` on the affected STABLE operations, and the changed error precedence. A
  `docs/upgrade.md` entry names each change, who is affected (clients sending login bodies above
  the D3 value; clients that do not retry 503; clients relying on 400 before 401/404) and the
  migration (retry 503 after `Retry-After`). `nurtuoctl` honors `Retry-After` with bounded retries in
  the same release. The `oasdiff breaking` check is expected to flag the login 413; the PR links
  this spec. The emergency security exception is not used unless an advisory is issued.
- D14's 503 appears only under concurrent load (other reservations hold the budget); on an idle
  instance every request that succeeds on `main` succeeds (load-test gate).
- Connection and secret routes are unmerged, so D8/D9/D12 do not affect released clients. They do
  change approved canvas Run D3/D4/D6/D11, the pending OpenAPI, `@nurtuo/client` generated types and
  the UI tests, which change together in #90. Any trial data with unbound credentials is treated as
  `missing` and needs re-entry; a stored `known_hosts` connection is refused until re-trusted with a
  fingerprint.

## Security

- Pre-auth memory: no body read before authorization except bounded D3 parsers. Per-peer in-flight
  limit and failed-auth budget use `NURTUO_TRUST_PROXY` semantics; key cardinality is bounded.
- Credential binding as above. Trusted TLS is required for credential-bearing PUTs (proxy-asserted
  or loopback). Login has no such server-side check today; adding one is out of scope.
- SSRF: canvas D11 applies at the actual dial, for retries and both address families (D12);
  `known_hosts` execution fails closed.
- Large-body staging, if L2 adds it later, holds customer content and needs private access, bounded
  retention and cleanup after crashes. Isolation workers (D14) never write content to disk.

## Implementation plan

No phase starts before Łukasz approves this spec. Each phase is a separate PR with its tests.

### Phase P90: PR #90 rework (must land in #90)

Findings from the Codex review (issuecomment-7134020883) and the Astra review (issuecomment-7134187185).
The second blocker below comes from the Astra review.

| Item | Finding | Change | Verify |
|---|---|---|---|
| P90-1 | Codex blocker / Astra blocker 1: password released to a repointed destination | D8, D10, D11, D13 binding, publication rule, credential lifecycle, 409 codes, `/secrets` removal (D9) | zero-auth tests T1-T6, T17-T20 |
| P90-2 | Astra blocker 2: missing scan tests | zero authentication during scan; scan deadline and socket close against a silent TCP peer | T7, T8 in `flowrun_sftp_test.go` and `runner.test.ts` |
| P90-3 | Codex major: DNS holds slots | deadline before preflight; `dns.promises.Resolver` (c-ares) with `timeout`/`tries` and `cancel()` on the run's abort signal (or a separately bounded pool); admission before graph validation (Astra major `runner.ts:87`) | T9 stalled resolver on both routes |
| P90-4 | Codex major + Astra majors `ConnectionsPanel.tsx:77/83`: edit discards settings / non-atomic save | single credential-bearing PUT; preserve untouched settings and refs; failed save keeps prior pair | T10, canvas test with an API-created connection |
| P90-5 | D1: global cap rejected | remove the global 1 MiB limit at `api.ts`; route-aware 413 for `interactive-run`, `scan` and the whole connection PUT (`secret-bearing`), decided from method and path before reading, with envelope values from the formulas | T11, T12, T21 |
| P90-6 | D12 | vetted-address pinning for `fingerprint` connections and scans at every dial; `known_hosts` fails closed; `known_hosts` paste converted to fingerprint | T13, T22 |
| P90-7 | Astra majors: Stop status, late workspace response, logout revocation | as listed in the Astra review | existing #90 test plan |
| P90-8 | Codex nit / Astra minor: preview decoding | fatal streaming UTF-8 on the truncated prefix | malformed large payload; code point split at boundary |
| P90-9 | D5 (required for #90) | `--max-output-bytes`, `--max-documents` in the child; runner passes D2's values | T23 |
| P90-10 | spec amendment | update canvas Run spec D2 scope, **D4** (`known_hosts` paste), **D11** (egress at the actual dial), Phase 2, Contracts, Compatibility, Gaps #3/#7/#9/#19 and changelog; add the D4/D5 rows to the compatibility contract | `node scripts/check-docs-links.mjs` |

### Phase L1a: admission and memory budget (separate issue, independent of #90)

Auth before body, route-inventory profiles plus coverage test against OpenAPI, unknown routes not
read, unread-body handling, D3 bounded parser, explicit `headersTimeout`/`requestTimeout`/
`maxHeaderSize`, per-peer and instance-wide ingress budgets and rate budgets (D7), D14 memory budget
with size-first reservation and isolation, D4 configuration surface with startup validation,
metrics, OpenAPI and upgrade note. Release gated on the D14 load tests (T24).

### Phase L1b: list streaming (separate issue)

Cursor or bounded iterator on `PgRepository` and `MemoryRepository`, byte-identical envelope,
per-row reservation, backpressure, abort on mid-stream failure.

### Later (separate specs)

- L2 revision storage redesign (D6), gated by metrics and a digest parity corpus.
- Runtime listener follow-up (V10). Required acceptance scope: per-trigger body limit as an additive
  `nurtuo.flow/v1` field bounded by a `runtime.yaml` ceiling; **webhook bursts**: per-trigger and
  instance in-flight limits, spool disk budget, bounded accept queue, 429/503 with `Retry-After`
  under burst, HMAC verification before dispatch kept, and load tests for bursts of small and large
  webhooks with recovery afterwards.
- `known_hosts` dial-address mechanism (D12): connector dial address separate from the verified
  host, `nurtuo flow` flag, socket tests as T22 plus logical-host verification with a pinned address.
- HA: cross-replica quotas, shared secret backend (D7).

## Acceptance criteria

- WHEN an unauthenticated client sends any body to any non-`preauth` route THEN the server answers
  401/404/405 with `Connection: close`, the application consumes no body bytes, server buffering is
  bounded by the socket high-water mark, and process RSS does not grow with body size.
- WHEN a body exceeds the `preauth` limit THEN 413 before parsing (if D3 approved).
- WHEN an existing client posts and fetches a revision above 1 MiB (fixed length and chunked, both
  digest modes, memory and PostgreSQL) THEN it gets 201 / 200 with full content and the same digest
  as on `main`.
- WHEN the list holds several revisions whose total exceeds the threshold THEN the response is
  byte-identical in meaning to `main` and is never truncated or omitted.
- WHEN a body, row or list would be materialized THEN its reservation is taken first; WHEN no
  reservation is free before the wait deadline because of other reservations THEN 503
  `capacity_unavailable` with `Retry-After`; every reservation is released exactly once.
- WHEN a single operation is larger than the large-operation budget THEN it is admitted exclusively,
  not rejected for its size.
- WHEN the D14 load tests run THEN the release succeeds at `S_main`, the process is not killed, the
  gateway keeps answering, and all reservations return to zero.
- WHEN many peers, rapid scans, stalled uploads and repeated timeouts or disconnects hit the instance
  THEN admission stays within the ingress budget and key table bound, and capacity returns to idle
  within the teardown grace.
- WHEN `POST …/test-runs` exceeds 1 MiB THEN 413; other D2 limits behave as in the canvas Run spec.
- WHEN the largest valid escaped connection PUT or scan request is sent THEN it succeeds; one byte
  more gets 413.
- **WHEN a credential bound to destination A is used with destination B, a different port, user,
  auth mode, host key or trust set, another connection, another field or another workspace THEN an
  unauthorized peer sees zero authentication attempts** (no TCP connection to it from a run that
  would reveal the credential), and the run returns 409 `credential_unbound` or the PUT returns 409
  `credential_rebind_required`.
- WHEN DNS answers change between preflight and dial, a retry dials, or the name has A and AAAA
  records THEN every TCP connection goes to the vetted address only; WHEN any answer is disallowed
  THEN no connection is made.
- WHEN the presented host key does not match THEN the peer sees zero authentication attempts.
- WHEN a stored `known_hosts` connection is run or scanned THEN 409 `dial_enforcement_unavailable` and
  no network contact.
- WHEN a run's child output exceeds D2's bytes or documents (large download or metadata-heavy listing)
  THEN the child stops at the limit with bounded memory and disk, the run fails `output_too_large`
  with no documents, and the temp dir and child are cleaned up.
- WHEN `secret://` appears outside a credential slot THEN save returns 400 `secret_ref_not_allowed`,
  and a hand-crafted stored row causes no child to start.
- WHEN a save fails (validation, conflict, authorization, database error) THEN the prior settings and
  credential still run; WHEN its outcome is uncertain THEN it is reconciled before the response.
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
`pnpm --filter @nurtuo/web run test`. Tests marked SFTP use the pinned CI SFTP fixture. "Socket
level" means a fake SSH or TCP peer that counts accepted TCP connections and `USERAUTH_REQUEST`
messages; a file check alone does not satisfy these tests.

| # | Test | Where | Notes |
|---|---|---|---|
| T1 | repoint host keeps password -> 409, fake SSH server at B counts 0 auth attempts | `core/control-plane/src/testrun/runner.test.ts` | SFTP; socket level |
| T2 | same for port, user, auth mode, host key, `knownHosts` text | same | table-driven |
| T3 | credential of connection A referenced by connection B / another field / another workspace -> rejected, 0 auth at B | `connections/validate.test.ts`, `runner.test.ts` | |
| T4 | `user: "secret://…"` -> 400 on save; stored row -> no child spawned | same | |
| T5 | delete/recreate same id -> old credential unusable | `runner.test.ts` | |
| T6 | concurrent same-binding rotations, multi-field update, run racing edit, failed Pg write | `connections/*.test.ts` | memory and Pg |
| T7 | scan: 0 auth attempts | `flowrun_sftp_test.go`, `runner.test.ts` | SFTP; socket level |
| T8 | scan against silent TCP peer closes at deadline | same | |
| T9 | stalled resolver on run and scan frees slot at deadline | `runner.test.ts` | injected resolver |
| T10 | edit with blank password keeps refs, `timeoutMs`, `maxListEntries` | `core/web` and `connections/routes.test.ts` | API-created connection |
| T11 | revision POST/GET/list above 1 MiB, chunked and fixed, both digest modes, both repositories | `api.test.ts`, `pg-*.test.ts` | Pg tests need a database |
| T12 | test-runs body > 1 MiB -> 413; scan and connection PUT: largest valid escaped request succeeds, +1 byte -> 413 (declared and chunked) | `api.test.ts`, `connections/routes.test.ts` | envelope formulas |
| T13 | fingerprint connection: binding uses the logical host; every TCP connection of the run goes to the vetted address | `runner.test.ts`, `flowrun_sftp_test.go` | socket level |
| T14 | unauthenticated large body on any route: application consumes 0 body bytes, `Connection: close`, server-side buffered bytes ≤ socket high-water mark, socket closed after the grace; `Expect: 100-continue` gets the final status | `api.test.ts` | L1a |
| T15 | every OpenAPI operation has a limit profile | `limits.test.ts` (new) | L1a |
| T16 | list streaming aborts on mid-stream failure, never completes partially | `api.test.ts` | L1b |
| T17 | credential PUT denials: bearer token (not user session), missing `secret:write` / `secret:delete`, missing CSRF, no trusted TLS (proxy without `https`, non-loopback without proxy) -> 403, nothing mutated | `connections/routes.test.ts` | |
| T18 | keep/set/remove per field; every valid and invalid SFTP combination; empty string; after 409 `credential_rebind_required`, re-entry succeeds and the run authenticates once at the new destination | `connections/routes.test.ts`, `runner.test.ts` | SFTP for the run |
| T19 | workspace deletion and connection deletion racing acquisition: either the admitted run finishes or the run is 404, never a reveal after purge | `connections/*.test.ts`, `runner.test.ts` | memory and Pg |
| T20 | uncertain commit (connection dropped after `COMMIT`): outcome reconciled before the response; unreadable generation -> 503 `save_outcome_unknown`, reconciled on next access; restart: Pg row kept, volatile versions `missing`, run refused without network contact, re-entry restores | `connections/pg-*.test.ts`, `runner.test.ts` | Pg needs a database |
| T21 | invalid D4 overrides (unknown key, bad unit, negative, raise of a safety ceiling, budget sum above `heap_size_limit` minus headroom) fail startup with a clear error | `limits.test.ts` | L1a |
| T22 | rebinding: DNS answer changes between preflight and dial; connector retry; A+AAAA with IPv4 and IPv6 listeners; one disallowed answer -> no connection; host-key mismatch -> 0 auth; stored `known_hosts` -> 409, 0 connections; pasted `known_hosts` with one / several keys -> fingerprint / 400 | `runner.test.ts`, `egress.test.ts`, `flowrun_sftp_test.go` | socket level, Run and scan |
| T23 | large SFTP download and metadata-heavy listing above D2 limits: child RSS and temp-dir high-water bounded, `output_too_large`, no documents, temp dir removed, child reaped | `flowrun_sftp_test.go`, `runner.test.ts` | SFTP |
| T24 | D14 load tests: `S_main` regression, high-expansion JSON, concurrent large and small reads and writes, slow readers, disconnects, uploads above `S_main`; gateway heartbeat answered; reservations return to zero; many peers, rapid scans, stalled uploads, repeated timeouts and disconnects with capacity recovery | `bench/` (new, `make bench`-style job) | L1a release gate |

## Gaps and risks

| # | Scenario | Severity | Area | Mitigation |
|---|---|---|---|---|
| 1 | Before L1a, an unauthenticated client streams 2 GiB to any path and the control plane (and its gateway) is OOM-killed | high | availability | L1a first; independent of #90 |
| 2 | With no default ceiling (D2), one authorized client posts a revision that exhausts the heap | medium | availability | D14 size-first reservation, isolation in a heap-limited worker and exclusive admission; operator ceiling; L2 |
| 3 | Two large uploads at once: the second gets 503 where it succeeded before | low | compatibility | D14 approval, `Retry-After`, `nurtuoctl` retries, threshold from benchmarks |
| 4 | Host key rotates; every run fails until someone who knows the password re-enters it | low | UX | accepted cost of binding trust material; UI explains |
| 5 | `known_hosts` connection: child re-resolves a rebinding name to `127.0.0.1` | medium | security | D12: refused (fail closed) until a dial mechanism is approved |
| 6 | Go child materializes a 4 GiB payload before the stdout cap | medium | reliability | D5 flags required for Run |
| 7 | Second control-plane replica uses a same-named local secret | medium | security | single owner in M1 (agreed item 11); HA spec |
| 8 | Initial thresholds (8 MiB, `k`, budgets, rate buckets) are wrong for real workloads | medium | performance | benchmark-gated; metrics from L1a |
| 9 | c-ares resolver ignores `/etc/hosts`/search domains, so a host that resolves for the system resolver fails in Run | low | UX | documented; CI uses IP literals with `NURTUO_TEST_RUN_ALLOW_PRIVATE=1` |
| 10 | Isolation worker transfer (structured clone) doubles memory for large trees | medium | performance | reservation counts it in `k`; child-process alternative chosen by measurement |
| 11 | A deployment behind a TLS proxy without `NURTUO_TRUST_PROXY=1` cannot save credentials (403 `tls_required`) | low | UX | documented in operations; the error names the setting |

## Council

Participants: GPT-6.2 Astra (OpenAI) and Claude Opus (Anthropic). GrokBot moderated. Statements:
Round 1 Astra, Round 2 Opus, Round 3 Astra. They are attached as comments to this spec's pull
request and are not committed (the council brief excludes `council/` from the repository). Opus
Round 2 and Astra Round 3 both conclude that no further round is needed. Astra R3: "The remaining
choices require Łukasz's approval or measurements. Another exchange of expert preferences would
supply neither."

The moderator confirmed that the Round-3 statement is Astra's (GPT-6.2 Astra, run 2026-11-24
17:20-17:22 Europe/Warsaw). The cross-provider review of this spec is recorded below; it is a review,
not a council round, and does not change the council outcomes.

Outcome: **agreed** = both experts hold the same position; **agreed with qualification** = Astra
R3 accepts Opus's position with conditions, which this spec adopts; **divergent** = positions
differ, owner decision. All three divergences (V2/D2, V3/D4, V9/D12) stay recorded.

| V | Topic | Astra position (R1 → R3) | Opus position (R2) | Outcome | Recommended default |
|---|---|---|---|---|---|
| V1 | First deliverable | R1: admission and Run controls first, storage redesign left open. R3 (modified): ship L1 first; authenticate before collecting protected bodies, isolate expensive operations, meter actual bytes; preserve stable revision sizes; include list streaming when the repository can supply rows incrementally; #90's security fixes proceed independently after approval | L1 (auth before body) first, independent of #90 | agreed with qualification | P90 and L1a in parallel after approval; L1b list streaming as a separate phase (D1, D7) |
| V2 | Revision ceiling | R1: "no new default byte cutoff" on existing endpoints, operator may opt in to a restrictive profile. R3 (disagree): reject a default 256 MiB ceiling on revision POST, GET or list. Creation, storage, deployment and aggregate readback use different representations. Whitespace disappears in `JSON.parse`, so the wire body can exceed the stored content with the same digest; no packing happens on POST; individually modest revisions can make a list above 256 MiB; a 413 on a bodyless GET misdescribes the failure; the owner requires compatibility with existing large uploads. A finite ceiling as an upgrade default needs a separate compatibility decision | R2 §3.2: explicit 256 MiB 413 on `POST /v1/components/{id}/revisions` **"and the matching response paths"**, derived from `MaxBundleSize`, `jsonb` and V8. R2's final ledger (§8, D-L2) and divergence table (§7, V2) speak of revision routes and the POST ceiling only. **Later withdrawal** (drafter revision in this spec, not a council round): the whitespace case is checkable on `main`, so a 256 MiB cap would shrink the contract; the drafter withdraws the default ceiling for POST and the response paths | **divergent** in the council record (R2 vs R3), kept as a historical divergence; the drafter's later revision aligns with Astra | No default ceiling; operator-configurable; D14 memory budget (D2) |
| V3 | Config surface | R1: versioned settings, profile defaults, per-operation overrides, rate and byte budgets, shared quotas for replicas. R3 (modified): operation-specific code defaults and one startup-validated class-level override surface for M1; keep instance budgets and D2 workspace reservations; allow explicit operator restrictions and evidence-backed tuning within physical capacity; "a tighten-only rule relative to an invented revision ceiling has no basis"; defer distributed quotas and general byte-rate controls | code defaults per operation, class-level overrides only, tighten-only except a bounded revision raise, no byte or workspace quotas in M1 | agreed with qualification on the surface; **divergent** on tighten-only | Class-level surface; raise allowed for declared tunable dimensions when evidence-backed, within enforced physical budgets (D4) |
| V4 | Status codes | R1: 413 size, 429 quota, capacity as a distinct transient failure. R3 (agree): 413 applicable size limit, 429 caller quota, 503 transient instance capacity, `Retry-After` on throttling or capacity exhaustion; the approved Run/scan concurrency stays 429 on its shared limiter, a 503 there would need an explicit amendment | 413 / 429 / 503 + `Retry-After` | agreed | 413 / 429 / 503; D2 stays 429 (D14) |
| V5 | Credential write API | R1: dedicated atomic credential-save operation, or a staged two-call handle. R3 (agree): optional write-only credentials on the pending connection PUT, retire the pending free-standing secrets API; revise approved D3/D6, OpenAPI, generated client and UI tests together | optional write-only `credentials` on the pending connection PUT; remove `/secrets` | agreed | PUT extension (D8, D9) |
| V6 | Binding representation | R1: immutable grant per credential version, staged versions, conditional generation publish, serialized publish and acquisition. R3 (modified): store the binding digest with each immutable credential version; checking and acquiring that exact version is one store operation; publish settings and version references consistently across concurrent saves, runs and deletes; no lock across network execution. "A digest comparison alone cannot make a database write and an in-memory replacement atomic" | binding digest stored with the credential, checked at reveal, no cross-path lock | agreed with qualification | Digest **plus** the publication rule (Design, D8) |
| V7 | Binding change without re-entry | R1: reject, or save an uncredentialed connection that cannot run. R3 (agree): reject with 409 `credential_rebind_required` before mutation; prompt for fresh values; preserve the working connection on failure | reject with 409 before mutation | agreed | Reject (D8) |
| V8 | `rootPath` | R1: bind it if the product promises subtree confinement. R3 (agree): exclude from the M1 binding; it scopes flow paths on an already authorized account; document that editors can change it; a future subtree promise needs a separate decision | do not bind; document as non-authorizing | agreed | Do not bind (D11) |
| V9 | DNS rebinding | R1: enforce at the actual dial or by network isolation. R3 (modified): pin a vetted dial address for fingerprint mode and scans; keep the logical SSH identity for known_hosts; enforce egress at every actual dial; approve a separate dial-address mechanism or equivalent network enforcement **before enabling that mode on a shared installation**; "best-effort preflight leaves the policy unenforceable" | pin the IP for `fingerprint` in #90; `known_hosts` best effort until a dial flag is approved | agreed on pinning; **divergent** on `known_hosts` meanwhile | Pin now; `known_hosts` refused everywhere (fail closed) until a mechanism is approved (D12) |
| V10 | Webhooks | R1: the receiving listener owns size and time policy. R3 (agree): keep production webhook policy with the runtime listener; commission a separate flow-format and runtime-configuration spec with per-trigger overrides and an instance ceiling | same; the knob does not exist yet (runtime fixed 1 MiB) | agreed | Separate runtime spec (non-goal here), scope including bursts (Later) |

## Cross-provider review

Reviewer: Codex (gpt-6.2-sol, OpenAI), https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7140661227,
on head `a8ad0c1`. Verdict: request changes (2 blockers, 5 majors, 1 nit). This revision is Claude
Opus's answer; it changes recommendations and design detail, not council outcomes, and approves
nothing.

| Item | Finding | Change in this spec |
|---|---|---|
| 1 (blocker) | D2/D14: no first-release memory bound for one operation | "Memory budget for the first release": byte-weighted budget, size-first reservation for fixed-length and chunked uploads, rows and lists, retained `MemoryRepository` history, isolation, exclusive admission, when 503 is allowed, load-test gate T24. No new default 413 (owner decision prevails) |
| 2 (blocker) | D12: `known_hosts` gate underspecified; T13 checks a file | Fail closed on every installation; API/UI behavior; canvas D4 and D11 amended; T13 and T22 at socket level (DNS change, retries, IPv4/IPv6, Run and scan, zero auth on key mismatch) |
| 3 (major) | D5 vs P90-9 | Producer bounds required for Run (every Run instance counts as shared); T23 |
| 4 (major) | D3/D14 compatibility path | One path: pre-1.0 deprecation protocol; error precedence documented; auth-disabled, D9 404 and SCIM covered; unread-body handling; T14 aligned |
| 5 (major) | D4 profile selection and override boundary | Whole connection PUT is `secret-bearing`; credential checks after bounded parse, before mutation; tunable vs safety ceilings; startup budget validation; envelope formulas gate P90; EXPERIMENTAL classification recorded in the compatibility contract; T12, T21 |
| 6 (major) | D7 churn and recovery | Instance-wide ingress budget, bounded queue and key table, targeted rate budgets, recovery criteria and T24; webhook-burst scope for the runtime follow-up |
| 7 (major) | D8/D9/D13 lifecycle | "Credential lifecycle": keep/set/remove, valid SFTP combinations, authorization per action, trusted TLS behind a proxy, uncertain-commit reconciliation before reporting, restart, deletion races; T17-T20 |
| 8 (nit) | Council provenance | V2 records Opus R2's response-path wording and the later withdrawal; review status updated |

## Open questions

1. Final D3 value for login after checking field sizes, encodings and existing clients.
2. Benchmarks for the D14 threshold, expansion factor `k`, budgets, ingress and rate placeholders, on
   both repositories. No production size distribution, PostgreSQL boundary measurement or load-test
   result has been supplied to the council (Astra R3).
3. Does any instance hold trial data in the #90 connection/secret tables that must be migrated, or
   is re-entry acceptable (assumed acceptable; absent or unbound volatile credentials are treated as
   `missing`)?
4. Name of the D4 configuration surface.
5. Is c-ares DNS (no `/etc/hosts`, different search resolution) acceptable for Run, or is a bounded
   threadpool resolver required?
6. Candidate `knownHosts` length cap (64 KiB) for the `secret-bearing` envelope.

## Improvement opportunities

- Additive `?content=false` on the revision list (ADDITIVE-ONLY), as a separate decision.
- `If-Match` on connection PUT for lost-update protection in the UI.
- Resumable chunked upload for bulk revisions, if clients need retries.
- A TLS listener in the control plane, which would remove the proxy/loopback rule in D8.

## References

- Council statements: Astra Round 1, Opus Round 2, Astra Round 3 (PR comments on this spec's pull
  request).
- Codex review and Astra review of PR #90, owner decision comment (links in Status).
- Cross-provider review of this spec (Status, "Cross-provider review").

## Changelog

- 2026-11-24: created as a council draft by Claude Opus. Status draft, needs owner decision.
- 2026-11-24: Astra's Round 3 (confirmed by the moderator) recorded. Council table now gives each
  V-item's outcome; D-list marks D2, D4 (override rule) and D12 (`known_hosts`) as divergent. Binding
  check now uses the snapshot's logical identity, not the child file after IP substitution. Status
  still draft, needs owner decision.
- 2026-11-24: Claude Opus's revision after the cross-provider review by Codex (gpt-6.2-sol,
  issuecomment-7140661227). Added the first-release memory budget (D2/D14), fail-closed `known_hosts`
  with canvas D4/D11 amendments and socket-level tests (D12), producer bounds as a Run condition (D5),
  one compatibility path with error precedence and unread-body handling (D3/D14), whole-PUT
  `secret-bearing` profile, override boundary, startup validation and stability classification (D4),
  ingress and rate budgets with recovery tests and webhook-burst scope (D7), and the credential
  lifecycle (D8/D9/D13). Council divergences D2, D4, D12 kept; V2 provenance clarified. Status still
  draft, needs owner decision.
