<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Control-plane memory limits for large operations

Status: draft (needs Łukasz's approval before any code). Date: 2026-11-26.

## TLDR

The physical memory bound and the byte-weighted reservation for large revision uploads, rows and
lists in the control plane, the chunked-upload spool and its disk budget, and the limits for
streamed readbacks. This material was part of the operation-limits spec (`docs/specs/2026-11-24-operation-limits-and-credential-binding.md` ([PR #92](https://github.com/nurtuo/nurtuo/pull/92))) until Łukasz
narrowed that spec on 2026-11-26. It is moved here unchanged and is a **draft**: the review
findings that were still open when it moved are listed under "Open questions".

This spec specifies the mechanism of approved decisions D2 and D14 (and the D4 dimensions they
use). It does not change those decisions. D numbers, the operation classes and the D7 ingress
budget refer to the operation-limits spec.

## Status and links

| | |
|---|---|
| Pull requests | this spec (draft); split out of [PR #92](https://github.com/nurtuo/nurtuo/pull/92) by Łukasz's decision of 2026-11-26. Refs [#90](https://github.com/nurtuo/nurtuo/pull/90) |
| Decisions it specifies | D2 (no default revision ceiling, memory bounded by D14), D4 group (b) (isolation threshold up to the coded maximum) and D14 (memory budget, reservation, isolation, 503 `capacity_unavailable`), approved by Łukasz on 2026-11-24 in `docs/specs/2026-11-24-operation-limits-and-credential-binding.md` ([PR #92](https://github.com/nurtuo/nurtuo/pull/92)) |
| Review history | Astra reviews on PR #92: https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7171018980, https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7171233823, https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7171586707, https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7171742890 (answered there), and https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7186855899 (open, see "Open questions"); Codex review https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7140661227 (item 1) |
| ADRs | [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (gateway co-hosted with the control plane) |
| Code (main `5a55cf0`) | `core/control-plane/src/api.ts` (`createHttpServer`), `core/control-plane/src/pg-repository.ts`, `core/control-plane/src/memory-repository.ts`, `core/control-plane/src/main.ts` |

## Problem and goals

On `main`, `createHttpServer` collects every request body, decodes it and runs `JSON.parse`
before it calls `handle()`. `GET /v1/components/{id}/revisions` returns every revision with full
content in one `JSON.stringify`. One large revision upload or list can therefore exhaust the heap
of the process that also hosts the runtime gateway. Approved D2 keeps uncapped revision writes and
full-content reads, so memory must be bounded without a new default 413.

- Goals:
  - A stated physical bound on process memory (`M_proc`), validated at startup.
  - Every body, row or list is reserved **before** it is materialized; the largest operations run
    isolated, and an operation above capacity fails alone, never the process or the gateway.
  - Chunked uploads above the in-memory threshold are spooled to disk under a bounded budget, so
    the isolated worker is sized from the final length.
  - Streamed readbacks never complete as a success after a failure.
  - Every request that succeeds on `main` on an idle instance still succeeds (load-test gate).
- Non-goals:
  - Authentication before the body read, operation classes, the D3 login parser, the D7 ingress
    and rate budgets and the credential binding. They stay in the operation-limits spec.
  - The revision storage redesign (L2, D6) and the runtime webhook listener (V10).

## Design

### Memory budget for the first release (L1a, D2, D14)

One large-body token does not bound process memory: a single admitted request still allocates
chunk buffers, the decoded string, the parsed tree, the canonical serialization and repository
copies. Worker heap limits alone do not bound it either: a worker's heap is outside the parent's
`heap_size_limit`, and the parent still holds socket chunks, database results and response copies
unless the design says where each byte lives. L1a therefore uses a **byte-weighted budget over a
stated physical bound**, reserved before allocation:

- **Physical bound.** Process memory is bounded by
  `M_proc = H_parent + E_parent + W + N`, where `H_parent` is the parent isolate's
  `heap_size_limit` (as configured today), `E_parent` is the parent's off-heap buffer budget
  (socket read chunks, PostgreSQL driver buffers of small requests, response write buffers),
  `W` is the **large-operation budget** (the sum over all workers of the heap share `h`, the
  `resourceLimits` young and old generation, code range and stack; the **external share** `x`,
  enforced by the pre-allocation gate below; and a measured fixed overhead `o` per worker) and `N` is the
  documented native overhead measured in L1a. Startup computes `M_proc` and refuses to start when
  it exceeds the D4 resource budget minus the gateway headroom; when a cgroup memory limit is
  present it is an upper bound for that budget. `W`, `E_parent`, the headroom and the pool sizes
  are startup-validated (D4).
- **Parent budget.** Requests below the in-memory threshold are charged to the small-request pool
  inside `H_parent` and `E_parent`. `MemoryRepository` retained history (the projected `rowBytes`
  of retained revisions, tracked at creation) is charged to `H_parent` continuously, because it is resident for
  the life of the process.
- **Estimate.** A worker reservation is `k × wireBytes` for a body (decoded string, tree, canonical
  string, repository copy) and `k × storedBytes` for a response, where `storedBytes` is the
  projected size of the rows served (see "Projected row size"). `k` is the measured expansion
  factor per class (placeholder, benchmark-gated with high-expansion JSON: deep nesting, many tiny
  objects, long escaped strings). A large operation also holds one parent transfer window (below).
- **Where the bytes of a large operation live.** Above the in-memory threshold the parent never
  holds the whole body, row or response:
  - *Upload.* The parent reads socket chunks into one bounded **transfer window** per large
    operation (placeholder 2 × 1 MiB, charged to `E_parent`) and transfers each chunk's
    `ArrayBuffer` to the worker (ownership transfer, not a structured-clone copy). The socket is
    paused while the window is full. Decode, parse, `canonicalJson` and digest run in the worker.
    For PostgreSQL the worker writes the row through its own database client (a small dedicated
    pool counted in `W`), so the parameter buffer lives in the worker. For `MemoryRepository` the
    canonical result is transferred to the parent and charged to retained history before it is
    accepted; a write that does not fit `H_parent` fails that request only (500
    `resource_exhausted`).
  - *Readback (single row and list).* The worker reads the row or rows through its own database
    client, so the driver's result buffers and parsed values live in the worker heap. It serializes
    the response and passes it to the parent through the transfer window; the parent writes to the
    socket and stops the worker's producer while the socket's `writableLength` is at the window
    size. For `MemoryRepository` the copy given to the worker is charged to the worker reservation.
  - *When the status is sent.* An isolated upload sends its 201 only after the write has
    committed, so every upload failure comes before headers and gets 500. An isolated readback
    sends its 200 only after the worker has loaded every row of the response and computed the
    exact response length in a counting serialization pass. That pass runs in slices of one
    window and keeps no output. The response then keeps the `Content-Length` framing it has on
    `main` (`res.end` of one string). Once the 200 has been sent, a 500 can no longer be
    promised. The **post-header abort rule** applies to every streamed readback from L1a on
    (single row and list), not only to L1b:
    - a worker failure after headers (heap limit, gate refusal, serialization error) makes the
      parent destroy the socket before `Content-Length` bytes have been written;
    - the client sees an incomplete response, never a complete success;
    - the failure is logged and counted as `resource_exhausted`, and every reservation is released
      exactly once.
  - Database results of small reads and the small-request pool are the only bodies the parent
    materializes, and they are bounded by the threshold.
- **Worker external memory (pre-allocation gate).** Worker `resourceLimits` cap only the V8 heap.
  `ArrayBuffer`s, `Buffer`s and native allocations of the PostgreSQL driver and of `crypto` are
  outside it, so a worker reservation is split into `h` (heap, set as `resourceLimits`), `x`
  (external) and `o` (fixed overhead), with `h = k × bytes` and `x = k_ext × bytes`. The transfer window is charged to `E_parent`, not
  to `x` (see below). `k_ext` is a measured code value like `k`. Every external allocation that grows
  with the payload is **checked against `x` before it is made**, and no allocation that is not
  checked may grow with the payload:
  - *Transfer window.* Credit-based: the parent sends the next chunk only after the worker has
    returned the credit for a consumed one, so at most the window is in flight. It is charged once,
    to `E_parent`, wherever its `ArrayBuffer`s currently are.
  - *String-to-bytes conversions* (digest input, response encoding) run in slices of at most one
    window, cut on code point boundaries; the worker never creates a whole-payload `Buffer`.
  - *PostgreSQL write.* The driver serializes the whole parameter into one growing buffer. The
    worker knows the canonical byte length before the `INSERT`; the gate charges
    `k_ext × length` against `x` and, if it does not fit, fails the request with 500
    `resource_exhausted` **before the query is issued**.
  - *PostgreSQL read.* The driver buffers one whole `DataRow` message. The projected size is read
    first (see "Projected row size", "Single row" and "Revision list"); the gate charges `k_ext × largest` plus one
    socket read chunk against `x` before the `SELECT`, and fails the request the same way.
  - The worker entry module may import only the modules listed in its allowlist (repository
    client, `pg`, `crypto`, the canonical JSON module); a test fails when the list grows, so a new
    external allocation path needs a reviewed gate.
  If L1a measurement finds an external allocation proportional to the payload that the gate cannot
  check in advance (for example a driver version that buffers differently), large operations run in
  the **child-process alternative** instead (see "Isolation"), with an OS-enforced memory limit per
  child (cgroup v2 `memory.max` or `RLIMIT_AS`) equal to its reservation, and the same tests.
- **Parent failure headroom.** The parent reserves a fixed `R_fail` inside `H_parent` and
  `E_parent` that no reservation can take. Detecting a worker or child failure, sending the 500
  (before headers) or aborting the response (after headers, see "When the status is sent"),
  closing the socket and releasing the reservation use only `R_fail`. So the failure is handled
  even when every reservation is in use.
- **Fixed-length upload.** With `Content-Length`, the full reservation is taken after
  authorization and **before the first body byte is read**. While waiting the socket is paused.
- **Chunked upload (bounded spooling before admission).** Worker `resourceLimits` are fixed when
  the worker is constructed, so a worker is never started before the body size is known. Without
  `Content-Length`, bytes are read into the small-request pool up to the in-memory threshold. A
  body that ends at or below the threshold (see the EOF probe below) is a small request. Past
  the threshold the parent writes the buffered prefix to a **spool file** and releases the prefix
  from the pool. It then keeps writing the body to the file without holding it in memory, one
  socket chunk at a time, charged to `E_parent`. Disk is charged to an instance-wide **spool
  budget** `S_spool`, reserved before more bytes are read. The socket is paused while a
  reservation is pending:
  - *Step target.* An upload that holds `held` bytes of spool and needs more asks for
    `target = min(2 × held, S_spool)`. Its first request is `min(2 × threshold, S_spool)`, which
    covers the prefix. A target is never above the budget, so a doubled step cannot ask for more
    than exists.
  - *Clamp to free capacity.* If only part of `target − held` is free, the upload takes what is
    free now (a partial grant) and goes on reading. A body that fits in the free space therefore
    never waits. The upload waits for the rest only when its bytes reach what it holds and more
    bytes remain.
  - *Exclusive use.* When the upload must wait (its bytes reached what it holds) and its target is
    the whole of `S_spool` while other spools hold part of it, it waits for exclusive use. New spool reservations queue behind it, as for memory.
  - *Bounded EOF probe.* At every boundary (the in-memory threshold, and `held` before another
    grant) the reader does not ask for more until it knows that more bytes exist. It reads at most
    one more socket chunk into the socket-chunk allowance of `E_parent` that it already holds.
    - If the body ends there (`end` with no data), the length is final and nothing more is
      reserved. So a body exactly at the threshold stays a small request, and a body exactly at
      `S_spool` succeeds.
    - If data arrives, the chunk stays in that allowance, with the socket paused, until the next
      grant.
  - *Above capacity.* An upload that holds the whole of `S_spool` and whose probe returns data
    physically needs more than the budget. It fails alone with 500 `resource_exhausted` and
    `Connection: close`, and its spool is released.
  - *Wait deadline.* A grant that cannot be made before the wait deadline because other spools
    hold the budget gets 503, as below.
  The spool budget follows the memory rules: it is never a reason to reject a request for its own
  size. When the body ends, its exact wire length is known. The operation then takes the same
  worker reservation as a fixed-length upload of that length (exclusive admission if above `W`).
  Only then is its worker constructed, once, with limits from that reservation, and it reads the
  spool through the transfer window. `ENOSPC` fails that request only (500 `resource_exhausted`).
  Only classes that can produce a large operation (`document`, `control`) spool; `preauth`, `secret-bearing`,
  `scan` and `interactive-run` bodies are below their ceilings and are never spooled. The spool
  budget (placeholder 4 GiB, benchmark-gated) is a D4 dimension outside groups (a)-(c), so
  operators may only tighten it. Startup validates it against the free space of the temp
  directory and rejects a value below the in-memory threshold, because the prefix must fit. The
  load-test gate requires it to fit `S_main`.
- **Spool ownership (instance-private, liveness-aware).** Several control-plane processes can
  share one temp directory, for example a restarting process next to its successor or two
  instances on one host. So no process may delete a file it does not own:
  - *Private directory.* At startup each process creates its own spool directory,
    `mkdtemp(join(os.tmpdir(), "nurtuo-spool-"))`, with mode `0700`. This needs no new setting. The
    process writes spool files only there. It never opens, lists or unlinks files in another
    process's directory.
  - *Unlinked at open.* A spool file is created with `O_CREAT | O_EXCL`, mode `0600` and an
    unguessable name. It is **unlinked right after it is opened, before the first body byte is
    written**. From then on the process uses only the open descriptor: it writes the body, then
    reads it back through the transfer window. A live spool file therefore has no name that
    another process could delete. A crash leaves no content behind, because the kernel frees the
    blocks when the descriptor closes. Closing the descriptor on completion, abort, disconnect,
    deadline and worker failure frees the space, and the spool reservation is released exactly
    once.
  - *Lease.* To tell abandoned directories from live ones, the owner touches a `lease` file in its
    directory every 10 s (code value).
  - *Sweep.* A sweep at startup and every minute removes only other `nurtuo-spool-*` directories
    whose lease is older than the stale age. The stale age is 10 min (code value), longer than the
    300 s `requestTimeout` plus the wait deadline. A directory with a fresh lease is never
    removed. Because live files are already unlinked, a wrong removal could take only names,
    never the bytes of a live upload. A process that finds its own directory missing recreates it
    before it opens the next spool file.
  - *No unlink of open files.* On platforms where an open file cannot be unlinked (Windows), the
    file keeps its name inside the private directory until it is closed, and the lease rule alone
    protects it.
  - *Shared disk.* Startup checks the spool budget against free space, but processes that share
    a disk can still oversubscribe it. `ENOSPC` then fails only the request that hit it (Gaps #14).
- **Projected row size (readback sizing).** Every readback is sized by the **full projected row**,
  not by content alone. The repositories return whole revision rows (`SELECT *`: `id`,
  `component_id`, `number`, `digest`, `content`, `created_at`), and the response serializes all of
  them. So many tiny revisions whose content alone fits under the threshold can still exceed the
  parent pool through IDs, digests, timestamps and per-row objects. The projected size of one row is
  `rowBytes = octet_length(content::text) + 6 × (octet_length(id) + octet_length(component_id) + octet_length(digest)) + c_row`:
  - the factor 6 is the `escapedMax` worst case for the short text columns;
  - `c_row` is a code value measured in L1a like `k`. It covers the JSON keys and punctuation, the
    number, the formatted timestamp, and the per-row overhead of driver row objects and the mapped
    `Revision` object.
  A list is sized as `total = envelope + Σ rowBytes`, together with `largest = max rowBytes`,
  `count` and `max(number)`, all **in one query in the sizing snapshot**. `MemoryRepository`
  computes the same `rowBytes` at creation from the stored row and keeps it with the revision.
  The threshold decision (parent pool or worker), the reservation `k × total`, and the read gate's
  `k_ext × largest` use these projected sizes. A size column added by migration may replace the
  `octet_length(content::text)` term if L1a measures that it is cheaper. It stores the same value.
- **Single row.** `GET …/revisions/{n}` and every internal read of one revision (deploy, digest
  checks) first read the projected size of that row, reserve, then load the row. Revisions are
  immutable and never deleted (migration `0001`), so the size read and the row read need no shared
  snapshot.
- **Revision list (one snapshot).** Before L1b, the size and the rows come from **one consistent
  read**, so an append between the two reads cannot exceed the reservation. Sizing is owned by the
  parent, because the reservation decides the worker's limits; loading is owned by whoever serves
  the response (the parent below the threshold, the worker above it). PostgreSQL:
  - The parent opens a read-only `REPEATABLE READ` transaction on one connection of its own pool
    and runs the size query: `total`, `largest`, `count` and `max(number)` of the projected row
    sizes for the component (see "Projected row size").
  - Below the threshold the parent loads, in the same transaction, only rows with
    `number <= ` that maximum, ordered by `number`, then commits.
  - Above the threshold the worker uses its own connection, and a second transaction on another
    connection does not share the parent's snapshot. So the parent calls `pg_export_snapshot()` in
    its sizing transaction and passes the snapshot id and the `number` cutoff to the worker. The
    worker's first statements are `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY` and
    `SET TRANSACTION SNAPSHOT '<id>'`, so it sees exactly the parent's snapshot. Only then does it
    load rows with `number <= ` the cutoff. The parent **keeps its sizing transaction open until
    the worker acknowledges the import** and then ends it. The worker's own transaction holds the
    snapshot from that point on.
  - The open sizing transaction is bounded by the request's wait deadline. Its connection is
    counted in the parent pool and released exactly once, including on worker failure, disconnect
    and deadline.
  - If the import fails (for example the exporting transaction was ended by a server-side
    `idle_in_transaction_session_timeout`), the worker issues no content query. The request fails
    before headers with 500 `resource_exhausted` and every reservation is released.
  - The `number` cutoff stays as a second guard. Revisions are immutable and never deleted
    (migration `0001`: update trigger, `ON DELETE RESTRICT`), so the cutoff names the same rows in
    any snapshot that contains the maximum.
  Memory: the size and the row count are captured in one synchronous step, and only that prefix of
  the append-only revision array is serialized, or copied to the worker under its reservation. A
  revision committed after the snapshot is not in this response. It is in the next one, as with any
  read that precedes a write. L1a reserves `k × total` before loading. L1b replaces this with an
  iterator that reserves per row (largest row plus write buffers) inside the same snapshot. The
  list is never truncated or omitted.
- **Responses.** A response reservation is held until the client drains it or disconnects. Slow
  readers keep their reservation; that is why the wait deadline and the instance-wide ingress
  budget (D7) exist.
- **Isolation.** The large-operation work above runs in a worker thread whose `resourceLimits`
  equal its heap share `h` and whose external allocations pass the gate against `x`. A worker heap
  failure (`ERR_WORKER_OUT_OF_MEMORY`) or a gate refusal ends that worker and that request only
  (500 `resource_exhausted` before headers, or an aborted response after them under the
  post-header abort rule), never the process or the gateway. On `main` the same request would kill
  the whole process, so no request that succeeds today is lost by this (the load-test gate below
  proves it). A child process is the alternative if measurement shows worker transfer cost is too
  high; L1a chooses by measurement, and the same physical bound and byte locations apply to it.
- **Exclusive admission.** A reservation larger than `W` is not rejected (owner decision prevails:
  no new default 413). It waits for exclusive admission: new large reservations queue behind it,
  it starts when `W` is idle, and it runs isolated in one worker whose `h + x + o` is the **whole
  of `W`**, never more. The process bound `M_proc` therefore holds for it too. If the operation
  physically needs more than `W`, its worker reaches its heap limit or the gate refuses an external
  allocation, and that request alone fails with 500 `resource_exhausted` before headers, or with an
  aborted response under the post-header abort rule (request-only failure above capacity); the parent, the gateway and
  other requests continue. The load-test gate requires `W` to fit the operations at `S_main`.
- **When 503 is permitted.** Only when a reservation cannot be obtained before the wait deadline
  because **other** reservations hold the budget (transient). Never because of the request's own
  size. The wait deadline is the remainder of the class `requestTimeout`. A 503 carries
  `Retry-After` and `Connection: close`. Each reservation is released exactly once, including on
  disconnect, abort and worker failure.
- **Load-test gate (blocks L1a release).** On the reference configuration: (1) establish `S_main`,
  the largest revision POST, GET and list that succeed on `main` with an otherwise idle instance;
  the release must succeed at `S_main` too, with the same digest. (2) With high-expansion JSON,
  concurrent large and small reads and writes, slow readers, mid-upload and mid-response
  disconnects, and an **oversized upload and an oversized readback** (single row and list, each
  needing more than `W`, one exhausting the heap share and one exhausting the external share, in
  both directions), the process is not killed, the gateway's mTLS listener keeps answering
  its heartbeat within its existing timeout, **peak process RSS stays within `M_proc`**, the
  oversized operations fail alone (500 `resource_exhausted` before headers, or an aborted response
  after them) while concurrent small requests
  succeed, and all reservations return to zero afterwards.

### Streaming readbacks

- **Request bodies.** L1a reads only after authorization and reservation. For `document` and
  `control`, bodies below the threshold stay in memory. Above it they are parsed in isolation as
  above. A true incremental parse of a revision is L2 (D6), because `canonicalJson` must keep its
  exact semantics (UTF-8 replacement before parse, last duplicate key wins, `__proto__` as own key,
  `Number`/`String` round trip, UTF-16 key order, lone surrogates escaped).
- **List response.** Writing the existing array one item at a time removes the one large string.
  The full fix is a cursor or bounded iterator with a consistent read snapshot that keeps the `items`
  envelope, order and full content, honors backpressure and closes on disconnect. A failure after
  headers are sent must abort the stream. A partial list must never complete as a success. This
  post-header abort rule already applies to L1a's isolated readbacks, which stream through the
  transfer window (see "When the status is sent"). Phase L1b adds the iterator; until then L1a's
  size-first reservation applies.

### Failure modes

| Case | Response |
|---|---|
| spool write fails (`ENOSPC`) or a spooled body needs more than the whole spool budget | 500 `resource_exhausted` for that request only, spool file removed (D14) |
| reservation not available before the wait deadline because other reservations hold the budget | 503 `capacity_unavailable`, `Retry-After` (D14) |
| isolated worker exceeds its heap, or the pre-allocation gate refuses an external allocation, before response headers | 500 `resource_exhausted` (sent from `R_fail`), process and gateway unaffected (D14) |
| isolated readback fails after the 200 headers were sent | response aborted: socket destroyed before `Content-Length` bytes, never a complete success; logged as `resource_exhausted`; reservations released; process and gateway unaffected (D14) |

## Contracts

No contract beyond those recorded for D2, D4 and D14 in the operation-limits spec: the possible
503 `capacity_unavailable` and 500 `resource_exhausted` on STABLE routes (pre-1.0 deprecation
protocol) and the spool budget as a tighten-only D4 dimension outside groups (a)-(c). The spool
needs no new configuration key; files go under `os.tmpdir()`.

## Compatibility

D14's 503 appears only under concurrent load (other reservations hold the budget); on an idle
instance every request that succeeds on `main` succeeds (load-test gate, T24). Isolated readbacks
keep `main`'s `Content-Length` framing and are byte-identical to `main`.

## Security

- The chunked-upload spool holds component content on disk: a per-process `0700` directory,
  `0600` files unlinked right after open (no name, freed by the kernel on crash), the spool budget,
  and a lease-based sweep that never touches a live process's directory ("Spool ownership"). Open
  question 2 records that the Windows fallback does not meet this yet.
- Isolation workers (D14) never write content to disk; the parent writes the spool. Large-body
  staging, if L2 adds it later, needs the same private access, bounded retention and cleanup after
  crashes.

## Implementation plan

No code until Łukasz approves this spec (status `approved`).

- **L1a memory part**: the budget, reservations, isolation, gate, spool and post-header abort above,
  in the same release as the operation-limits L1a admission work. Release gated on T24.
- **L1b list streaming** (separate issue): cursor or bounded iterator on `PgRepository` and
  `MemoryRepository`, byte-identical envelope, per-row reservation, backpressure, abort on
  mid-stream failure.

## Acceptance criteria

- WHEN a revision is appended between the size read and the row read of a list THEN the list
  stays within its reservation and contains exactly the snapshot's revisions, also when the list
  is above the isolation threshold and the rows are read by the worker on its own connection
  (exported and imported snapshot).
- WHEN a list or row is read back THEN it is sized by the full projected rows (all returned
  columns and per-row overhead, total and largest row from one snapshot) before the parent pool or
  a worker is chosen; a list of many minimal revisions whose content alone fits under the
  threshold but whose full response exceeds it is served by a worker.
- WHEN an upload or a readback needs more than `W` THEN only that request fails (500
  `resource_exhausted` before headers; for a readback that fails after its 200 headers, an
  aborted response that is never a complete success), RSS stays within `M_proc` and the gateway
  keeps answering.
- WHEN an isolated readback is streamed THEN its 200 and `Content-Length` are sent only after all
  rows are loaded and the exact length is counted, and the body equals `main` byte for byte.
- WHEN a large operation's driver or encoding buffers would exceed its external share `x` THEN the
  request fails with 500 before the allocation (no content query issued), the parent sends the 500
  from its reserved headroom, and RSS stays within `M_proc`.
- WHEN a chunked upload crosses the in-memory threshold THEN it is spooled to disk under the spool
  budget in a private per-process spool directory, its worker is constructed only after the last
  byte with limits for the final length,
  and it gets the same result and digest as the same body sent with `Content-Length`; no spool
  grant exceeds the free budget, and a body exactly at the threshold or exactly at the spool
  budget succeeds.
- WHEN the list holds several revisions whose total exceeds the threshold THEN the response is
  byte-identical in meaning to `main` and is never truncated or omitted.
- WHEN a body, row or list would be materialized THEN its reservation is taken first; WHEN no
  reservation is free before the wait deadline because of other reservations THEN 503
  `capacity_unavailable` with `Retry-After`; every reservation is released exactly once.
- WHEN a single operation is larger than the large-operation budget THEN it is admitted exclusively,
  not rejected for its size.
- WHEN the D14 load tests run THEN the release succeeds at `S_main`, the process is not killed, the
  gateway keeps answering, and all reservations return to zero.

## Testing

Commands: `pnpm --filter @nurtuo/control-plane run build && pnpm --filter @nurtuo/control-plane
run test`; the T24 load tests run in a new `bench/` job. Test numbers are kept from the
operation-limits spec so earlier review comments stay readable. T11-M and T21-M are the memory
cases split out of that spec's T11 and T21.

| # | Test | Where | Notes |
|---|---|---|---|
| T11-M | a revision appended between the list's size read and row read (injected hook in both repositories) is not in that response and the reservation is never exceeded; **a list above the isolation threshold** with a revision appended (and committed) between the parent's sizing query and the worker's content query: the worker imports the parent's exported snapshot, the response holds exactly the snapshot's revisions, and the parent's sizing transaction ends only after the import is acknowledged; an import that fails (exporting transaction ended by an injected `idle_in_transaction_session_timeout`) gives 500 before headers with no content query issued (statement log) and all reservations and the parent connection released | `api.test.ts`, `pg-*.test.ts` | Pg tests need a database |
| T16 | list streaming aborts on mid-stream failure, never completes partially | `api.test.ts` | L1b |
| T21-M | a memory budget whose `M_proc` exceeds the D4 resource budget minus the gateway headroom (or a cgroup memory limit), and a spool budget below the in-memory threshold or above the temp directory's free space, fail startup with a clear error | `limits.test.ts` | L1a |
| T24 | D14 load tests: `S_main` regression, high-expansion JSON, concurrent large and small reads and writes, slow readers, disconnects, uploads above `S_main`; oversized upload and oversized readback (single row and list) above `W` fail alone with 500 `resource_exhausted` before headers (a readback that fails after its 200 is aborted, see T31); **external-buffer exhaustion in both directions**: an upload whose PostgreSQL parameter (`k_ext × canonical length`) exceeds `x` and a readback whose largest row exceeds `x` while the heap share still fits, each failing with 500 before the query is issued (statement log shows no `INSERT`/`SELECT` of content), with every reservation in use so the 500 comes from `R_fail`; worker `arrayBuffers` and `external` high-water ≤ `x` for every passing large operation; **many minimal revisions** (for example 200,000 revisions with content `{}`) whose summed `content::text` is far below the threshold but whose full list response exceeds it: in both repositories the projected `total` exceeds the threshold, the list is served by a worker and not from the parent pool, the parent's heap and external high-water stay within the small-request pool, and the measured response size never exceeds `total`; peak process RSS ≤ `M_proc`; parent holds at most one transfer window per large operation; gateway heartbeat answered; reservations return to zero | `bench/` (new, `make bench`-style job) | L1a release gate |
| T29 | worker isolation unit tests: the worker entry module's import allowlist is fixed (a new import fails the test); the gate refuses before the query when `k_ext × bytes` exceeds `x` for write and read, with an injected PostgreSQL client that records issued queries; digest and response encoding never allocate a `Buffer` larger than one window (instrumented `Buffer.alloc`/`TextEncoder`), including a code point split at a slice boundary; at most one window in flight under credit flow; `R_fail` is not available to reservations | `limits/worker.test.ts` (new) | L1a |
| T30 | one revision (high-expansion JSON, several times the threshold) uploaded **chunked** across several spool growth steps, with slow trickle and with large chunks, and **fixed length**: both give 201 and the same digest as on `main`, in both repositories; for the chunked upload no worker exists before the last byte, exactly one worker is constructed; the reservation (`h`, `x`, `o`) is identical for the chunked and the fixed-length upload of the same body, and in both modes the worker's `resourceLimits` equal the heap share `h` only (not `h + x + o`); each spool step is reserved before more bytes are read (socket paused while waiting); readback returns the same content; spool held by other uploads until the wait deadline -> 503 with `Retry-After`; body above the whole spool budget waits for exclusive use; **boundaries**: a body exactly at the in-memory threshold stays a small request (no spool file, no worker) and threshold + 1 byte is spooled; a body between growth boundaries (for example 3 × threshold) succeeds, with the final spool grant clamped to `S_spool` and the worker reservation taken from the final wire length, not from the grant; with other spools holding part of the budget, a body that fits in the free space succeeds through a partial grant without waiting; a body exactly at `S_spool` succeeds (EOF probe, nothing reserved above the budget); `S_spool` + 1 byte fails alone with 500 `resource_exhausted` after exclusive use; the spool reservation high-water never exceeds `S_spool` and the probe never holds more than one socket chunk; mid-upload disconnect, worker failure, injected `ENOSPC` and restart each leave no spool file and return the spool and memory reservations to zero; spool directory `0700`, spool files `0600` and without a directory entry after open (directory listing empty during an upload); **spool ownership**: a second instance started mid-upload on the same temp directory leaves the first upload intact (201, same digest), and its startup sweep removes nothing of the live instance; a process killed (`SIGKILL`) mid-upload while a second process completes its own upload: the second upload completes with the same digest, the killed process leaves no spool content, and its directory is removed only after its lease is stale, never while the lease is fresh | `api.test.ts`, `pg-*.test.ts`, `limits/spool.test.ts` (new) | L1a; Pg tests need a database |
| T31 | post-header abort for isolated readbacks (single row and list, both repositories): an injected worker failure before headers (while loading rows, and in the counting pass) gives 500 `resource_exhausted`; an injected failure after the first chunk is written gives an incomplete response (socket closed before `Content-Length` bytes; the client reports a truncated body, never a 200 with a complete body); in both cases the worker, the parent connection and every memory reservation are released exactly once and the next request succeeds; a successful isolated readback sends `Content-Length` equal to the bytes written and byte-identical to `main` | `api.test.ts`, `pg-*.test.ts`, `limits/worker.test.ts` | L1a; Pg tests need a database |

## Gaps and risks

Numbers are kept from the operation-limits spec.

| # | Scenario | Severity | Area | Mitigation |
|---|---|---|---|---|
| 2 | With no default ceiling (D2), one authorized client posts a revision that exhausts the heap | medium | availability | D14 size-first reservation, isolation in a heap-limited worker and exclusive admission; operator ceiling; L2 |
| 3 | Two large uploads at once: the second gets 503 where it succeeded before | low | compatibility | D14 approval, `Retry-After`, `nurtuoctl` retries, threshold from benchmarks |
| 8 | Initial memory values (8 MiB threshold, `k`, `k_ext`, `c_row`, `W`, `E_parent`, spool budget) are wrong for real workloads | medium | performance | benchmark-gated; metrics from L1a (open question 4) |
| 10 | Isolation worker transfer (structured clone) doubles memory for large trees | medium | performance | reservation counts it in `k`; child-process alternative chosen by measurement |
| 13 | A worker allocates external memory (driver, `crypto`, encoding) that its heap limit does not cap, and the shared process is OOM-killed before the parent can answer | high | availability | pre-allocation gate against `x` with `k_ext`, sliced conversions, import allowlist, `R_fail`; T24, T29; child process with an OS memory limit if a path cannot be gated |
| 14 | Spooled chunked uploads fill the disk, leave component content on disk, or one process deletes another's spool | medium | availability, confidentiality | instance spool budget validated at startup against free space (processes sharing a disk can still oversubscribe it; `ENOSPC` fails the request only); per-process `0700` spool directory; files `0600` with unguessable names, unlinked right after open so a crash leaves no content; lease-based sweep removes only stale directories of other processes; T30 |
| 15 | A large list holds a parent PostgreSQL connection idle in transaction until the worker imports the exported snapshot; a server `idle_in_transaction_session_timeout` or an exhausted parent pool ends or delays it | low | availability | the hold lasts only until the import is acknowledged and is bounded by the wait deadline; the connection is counted in the parent pool; a failed import fails that request before headers (500) with no content query; T11 |

## Open questions

The Astra review of PR #92 head `40c27e3`
(https://github.com/nurtuo/nurtuo/pull/92#issuecomment-7186855899, request changes) was still open
when this material moved. Its findings are recorded here unresolved. They block approval of this
spec.

| # | Finding (severity) | Question | Blocks |
|---|---|---|---|
| 1 | Blocker: for an isolated single-row GET within the normal worker budget, `bytes = rowBytes = largest`, so the reservation grants `x = k_ext × bytes`, but the pre-`SELECT` gate charges that amount **plus one socket read chunk** against `x` ("Worker external memory", *PostgreSQL read*). The gate then returns 500 even on an idle instance | Include the fixed driver chunk in the reservation and in the sum charged to `W`, or reserve it separately? Astra asks for a T24 case: a PostgreSQL single-row GET just above the isolation threshold and below `W` returns the original 200 and body with the gate enabled | approval; L1a |
| 2 | Major: on Windows a spool file keeps its name until close ("Spool ownership", *No unlink of open files*). After a crash the payload can stay on disk, and a paused live owner can outlast its heartbeat and be swept as stale. T30 requires an empty listing right after a kill, which this fallback cannot deliver | OS-enforced ownership or delete-on-close, plus a recovery deadline per platform? Test both a killed owner and a live owner paused beyond the stale age. On POSIX, should T30 allow the `lease` entry in the listing? | approval; L1a |
| 3 | Major: the partial-grant rule ("Chunked upload", *Clamp to free capacity*) also covers the first grant. If other uploads leave less than one threshold free, the grant cannot cover the buffered prefix | A minimum first grant equal to the prefix size, or incremental writes that keep the unwritten prefix in the parent pool? Extend T30 with free capacity below the prefix and assert both pool charges while waiting and after disconnect | approval; L1a |
| 4 | Benchmarks (moved from the operation-limits spec's remaining question 2) | Values for the D14 threshold, `k`, `k_ext`, `c_row`, `W`, `E_parent`, the pool sizes and the spool budget on both repositories. No production size distribution, PostgreSQL boundary measurement or load-test result has been supplied yet. Owner: L1a implementer; values recorded in the changelog | L1a release (T24 gate) |

## Improvement opportunities

- Additive `?content=false` on the revision list (ADDITIVE-ONLY), as a separate decision.
- Resumable chunked upload for bulk revisions, if clients need retries.

## References

- Operation-limits spec and its review history on PR #92 (see "Status and links").

## Changelog

- 2026-11-26: created by Claude Opus on Łukasz's decision to narrow PR #92. The memory budget
  (physical bound `M_proc`, reservations, isolation, pre-allocation gate, `R_fail`, exclusive
  admission, load-test gate), the chunked-upload spool and its budget, the streamed-readback rules,
  their failure modes, acceptance criteria, tests T11-M, T16, T21-M, T24, T29-T31 and Gaps 2, 3, 8,
  10, 13-15 are moved from the operation-limits spec at `40c27e3` without design changes. T24 no
  longer carries the D7 ingress-recovery cases, which stay in the operation-limits spec (T25). The
  open findings of the Astra review of `40c27e3` are open questions 1-3. Status draft.
