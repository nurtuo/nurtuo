<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Control plane REST API and data model

Status: implemented. Date: 2026-11-21 (written as built on 2026-11-22).

## TLDR

The control plane (`@nurtuo/control-plane`, TypeScript, AGPL) stores components and their immutable
revisions, registers runtimes, and turns deployments into a per-runtime **desired state** with a
generation counter and a transactional outbox. It is reachable through a transport-agnostic REST
handler (`createHandler`) and a thin `node:http` adapter. The one thing to know: **the data tables have
no `workspace_id`**. Every workspace sees and changes the same components, runtimes and deployments
(see [Gaps and risks](#gaps-and-risks) #1 and the draft spec `workspace-isolation`).

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/26 (squash commit `dcd60dc`) |
| ADRs | [ADR 0009](../../adr/0009-outbound-only-runtime-gateway.md) (runtimes dial out to the gateway) |
| Issues | none |
| Code | `core/control-plane/src/{domain,repository,memory-repository,pg-repository,api,gateway,main,index}.ts`, `core/control-plane/migrations/0001..0003`, `core/control-plane/openapi.yaml` |
| User docs | [auth.md](../../auth.md), [security.md](../../security.md), [operations.md](../../operations.md) |
| Related specs | `auth-rbac-audit` (the auth hook that wraps this API), `runtime-gateway-protocol-v1` (wire protocol of the gateway), drafts `workspace-isolation`, `auth-rate-limiting-and-lockout` |

## Problem and goals

Flow authors (humans, the canvas, the CLI, AI agents through MCP) need one place to store versioned
components and to say "run revision N of component C on runtime R". Runtimes need to learn that
change without the control plane ever connecting to them (ADR 0009).

- Goals:
  - Content-addressed, immutable revisions with server-assigned numbers.
  - Deploy and undeploy as a single transaction that bumps a per-runtime generation and writes an
    outbox row, so the gateway can push the change at least once.
  - Two interchangeable repositories (in-memory for tests/dev, Postgres for real use) behind one
    `Repository` interface.
  - A documented REST API (`openapi.yaml`) that the auth layer can check route by route.
- Non-goals (M1): multi-tenancy of data, pagination, server-side optimistic locking, bundle artifact
  delivery (`FetchArtifact`, see draft `bundle-artifact-delivery`), telemetry ingest.

## Design (as built)

### Layers

```
node:http  createHttpServer(repo, opts)          src/api.ts
   |        parses JSON, collects headers and peer ip
   v
createHandler(repo, { auth })                    src/api.ts
   |  auth === "disabled" -> route() directly (tests/dev only)
   |  else auth.handle(req, routeSafe)           src/auth/middleware.ts
   v
route(repo, req)  -> Repository                  src/api.ts
                      MemoryRepository           src/memory-repository.ts
                      PgRepository + migrate()   src/pg-repository.ts
Gateway(repo).startOutboxPublisher()             src/gateway.ts (mTLS, HTTP/2, Connect)
```

`ApiRequest`/`ApiResponse` are plain objects, so every test drives the handler in-process without HTTP
or a database. `ApiError(status, code, message)` (`src/domain.ts`) is mapped to
`{ "error": { "code", "message" } }`; any other exception becomes `500 internal`.

### Data model (migrations)

| Migration | Tables | Notes |
|---|---|---|
| `0001_components.sql` | `components(id, name UNIQUE, kind, description, created_at)`, `revisions(id, component_id, number, digest, content jsonb, created_at, UNIQUE(component_id, number))` | `kind` in `flow`, `map`, `connector`, `script`. Trigger `revisions_no_update` raises on any UPDATE (`revisions_immutable()`). `ON DELETE RESTRICT` from revisions to components. |
| `0002_runtimes.sql` | `runtimes(id, name, environment, status, version, labels jsonb, last_seen_at, created_at)` | `status` in `online`, `offline`; index on `environment`. |
| `0003_desired_state_deployments_outbox.sql` | `desired_state(runtime_id PK, environment, generation, items jsonb)`, `deployments(..., generation, status, message)`, `outbox(id bigserial, topic, payload, published_at)` | `items` maps componentId to revisionId. Partial index `outbox_unpublished_idx`. |

`migrate(db, dir)` (`src/pg-repository.ts`) creates `schema_migrations`, applies `*.sql` files in file
name order, one transaction per file, and skips names already recorded. There are no down migrations.
Auth migrations are numbered from `0100` (see `auth-rbac-audit`).

### Repository semantics (`src/repository.ts`)

- `createComponent`: name unique (409 `conflict`; Postgres maps SQLSTATE `23505`).
- `deleteComponent`: refused with `has_revisions` (HTTP 409) when any revision exists, so in practice a
  component that was ever saved can never be deleted.
- `createRevision`: Postgres locks the component row (`SELECT ... FOR UPDATE`) and inserts
  `MAX(number)+1` in the same transaction, so numbers are gap-free and race-free per component. The
  memory repository uses `length + 1` and `Object.freeze`.
- `deploy`: one transaction: upsert `desired_state` with `generation + 1` and
  `items || {componentId: revisionId}`, insert a `deployments` row with that generation, insert an
  `outbox` row `desired_state.changed` with `{runtimeId, generation}`. 404 when the runtime or the
  revision-for-component is missing.
- `undeploy`: `items - componentId` and `generation + 1` only if the component is present
  (`items ? $2`), plus an outbox row; returns `undefined` (HTTP 404) otherwise.
- `touchRuntime` / `markRuntimeOffline`: used by the gateway for liveness.
- `listOutbox` / `markOutboxPublished`: the latter is idempotent (`WHERE published_at IS NULL`).

### Digests

`canonicalJson` sorts object keys recursively; `digestOf(content)` is `sha256:<hex>` of that string. A
client that packages a signed bundle can supply its own `digest` (must match `^sha256:[0-9a-f]{64}$`);
the server does not recompute or verify it in that case.

### Gateway server (summary)

`createGatewayServer` (`src/gateway.ts`) is an HTTP/2 TLS server with `requestCert: true`,
`rejectUnauthorized: true` and `minVersion: "TLSv1.3"`. `runtimeIdFromCert` takes the identity from a
`spiffe://nurtuo/runtime/<id>` URI SAN, else the subject CN. The first stream message must be `Hello`
with a `runtime_id` equal to the certificate identity, and the runtime must already exist
(`touchRuntime` returns a row), otherwise `PermissionDenied`. The newest connection for a runtime
replaces the older one. `Gateway.publishOutbox` pushes the current desired state for every unpublished
`desired_state.changed` row (serialised, one run in flight) and marks rows published even when the
runtime is offline, because `Hello` reconciles on reconnect. Deployment status reports are mapped by
`STATE_MAP` to the newest deployment row at or below the reported generation. The message flow is
specified in `runtime-gateway-protocol-v1`.

### Entry point (`src/main.ts`)

`DATABASE_URL` set: `pg.Pool`, `migrate`, `PgRepository` + `PgAuthRepository`; otherwise both in-memory
(data is lost on restart). REST on `PORT` (default 8080). Gateway on `GATEWAY_PORT` (default 8443) only
when `GATEWAY_CERT`, `GATEWAY_KEY` and `GATEWAY_CA` are all set. Auth options are covered in
`auth-rbac-audit`.

## Contracts

| Contract | Stability | Notes |
|---|---|---|
| REST routes under `/v1/components`, `/v1/runtimes`, `/v1/deployments` (table below) | experimental | Documented in `core/control-plane/openapi.yaml` (`info.version: 0.1.0`), consumed by `sdk/ts/client`, `cli` and the MCP `ControlPlaneFlowStore` (`core/mcp/src/control-plane-store.ts`). |
| Error body `{ "error": { "code", "message" } }` | experimental | Codes used here: `bad_request`, `not_found`, `conflict`, `method_not_allowed`, `internal`. |
| Digest format `sha256:<64 hex>` of canonical JSON | experimental | Also the `bundle_digest` sent to runtimes. |
| Outbox topic `desired_state.changed` with `{runtimeId, generation}` | internal | Only the in-process gateway consumes it. |
| SQL schema (migrations `0001` to `0003`) | internal | Applied automatically at start-up; no down migrations. |
| Environment: `DATABASE_URL`, `PORT`, `GATEWAY_CERT`, `GATEWAY_KEY`, `GATEWAY_CA`, `GATEWAY_PORT` | experimental | Read in `src/main.ts`. |
| Package exports (`src/index.ts`): `createHandler`, `createHttpServer`, `MemoryRepository`, `PgRepository`, `migrate`, `Gateway`, `createGatewayServer`, `runtimeIdFromCert`, `digestOf`, `canonicalJson`, `auth` | internal | The package is `private: true`. |

| Method and path | Success | Permission (from `src/auth/policy.ts`) |
|---|---|---|
| `GET /v1/components` | 200 `{items}` | `component:read` |
| `POST /v1/components` `{name, kind?, description?}` | 201 | `component:write` |
| `GET`, `PATCH {description}`, `DELETE /v1/components/{id}` | 200, 200, 204 | `component:read`, `component:write`, `component:delete` |
| `GET`, `POST /v1/components/{id}/revisions` `{content, digest?}` | 200, 201 | `component:read`, `component:write` |
| `GET /v1/components/{id}/revisions/{number}` | 200 | `component:read` |
| `GET /v1/runtimes?environment=` | 200 | `runtime:read` |
| `POST /v1/runtimes` `{id, name, environment, version?, labels?}` (upsert) | 200 | `runtime:register` |
| `GET /v1/runtimes/{id}`, `GET /v1/runtimes/{id}/desired-state` | 200 | `runtime:read` |
| `DELETE /v1/runtimes/{id}/deployments/{componentId}` (undeploy) | 202 | `deployment:create` |
| `GET /v1/deployments?runtimeId=&componentId=` | 200 | `deployment:read` |
| `POST /v1/deployments` `{componentId, runtimeId, revisionId or revision}` | 202 | `deployment:create` |
| `GET /v1/deployments/{id}` | 200 | `deployment:read` |
| `PUT /v1/deployments/{id}/status` `{status, message?}` | 200 | `deployment:report` |

## Security

- Auth is a hook around the router; `api.ts` has no auth logic. `createHandler(repo)` defaults to
  `auth: "disabled"`, which opens every route; `main.ts` only uses it with `NURTUO_INSECURE_NO_AUTH=1`.
- Input validation: bodies must be JSON objects, names match `^[a-z0-9][a-z0-9._-]*$`, kinds and
  statuses are enums, revision numbers are positive integers. `labels` on `POST /v1/runtimes` is not
  type-checked (any JSON value is stored).
- The HTTP adapter does **not** bound the request body and does not terminate TLS (documented in
  [auth.md](../../auth.md#known-gaps)); run behind a reverse proxy.
- Gateway: client certificate required, TLS 1.3 only, identity bound to the certificate. TEST-ONLY
  keys for the tests are committed under `core/control-plane/testdata/certs` (generated by
  `testdata/gen-certs.sh`; the CA private key is not committed).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `component CRUD and validation`, `revisions are numbered, immutable and digest-addressed`, `deploy updates desired state, creates deployment and outbox, status reported`, `unknown routes`, `revision accepts an externally supplied artifact digest`, `undeploy removes the component from desired state and enqueues a change` in `src/api.test.ts` | handler + `MemoryRepository` | `pnpm turbo run typecheck test --filter=@nurtuo/control-plane` | auth disabled |
| `postgres repository: migrate, revisions, deploy+outbox` in `src/pg-repository.test.ts` | migrations, revision trigger, deploy/outbox, liveness on Postgres | `DATABASE_URL=postgres://... pnpm --filter @nurtuo/control-plane test` | **skipped unless `DATABASE_URL` is set**; the test drops the `public` schema; CI sets no `DATABASE_URL`, so it never runs in CI |
| `runtimeIdFromCert prefers URI SAN over CN`, `happy path: Hello reconciles, status persisted, heartbeat tracked`, `Hello whose runtime_id does not match the certificate is rejected`, `first message must be Hello`, `identity can come from a URI SAN (CN ignored)`, `client cert from an untrusted CA is refused at the TLS layer`, `desired state is pushed after deploy() via the outbox; matching revision gets no message`, `reconnect resends state missed while offline`, `a newer connection for the same runtime replaces the older one` in `src/gateway.test.ts` | gateway server over real mTLS with the TEST-ONLY certificates | same as above | |
| `store round trip against the control-plane handler`, `two stores racing on the same flow: exactly one wins`, `MCP server on the control-plane store: build, conflict, diff, deploy, status`, `over real HTTP with bearer auth` in `src/mcp-store.test.ts` | MCP `ControlPlaneFlowStore` against this API | same as above | the real-HTTP case runs with auth disabled; the bearer case uses an in-process fake |
| `every openapi operation has an authz policy, and every policy is documented` and the other cases in `src/auth/policy.test.ts` | `openapi.yaml` and the policy table stay in sync | same as above | see `auth-rbac-audit` |

The M1 review guide ([PR-STACK.md](../../review/PR-STACK.md)) says repositories were exercised against
fakes and "embedded PostgreSQL where available"; in this package there is no embedded Postgres, only
the two `DATABASE_URL`-gated tests above. The `lint` script in `core/control-plane/package.json` is
`echo 'lint: TODO (eslint)'`.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | `components`, `revisions`, `runtimes`, `desired_state` and `deployments` have no `workspace_id`; routes resolve a workspace only to check a permission. Any authenticated user can `POST /v1/workspaces`, becomes `owner` of the new workspace, sends `X-Nurtuo-Workspace: <new slug>` and then holds `component:delete` and `deployment:create` over **every** component and runtime of every other workspace (deploys to a production runtime, reads all flow contents). | critical | data model, authz | draft spec `workspace-isolation`; until then run one workspace per control plane and do not give untrusted users accounts |
| 2 | `createHttpServer` concatenates the whole request body before parsing; one client sending a multi-GB body (no auth needed, the body is read before the auth hook runs) exhausts memory and the process is OOM-killed. | high | HTTP adapter | add a body limit (for example 4 MiB, 413) and a request timeout in `createHttpServer`; keep a reverse proxy limit meanwhile |
| 3 | Both Postgres-backed tests are skipped in CI (no `DATABASE_URL`, no service container in `.github/workflows/ci.yml`). A SQL error in `PgRepository` (for example in `undeploy`'s `items ? $2`) or a broken migration ships unnoticed; the memory repository passes all handler tests. | high | testing | add a `postgres:16` service to CI and set `DATABASE_URL` for `@nurtuo/control-plane`; draft spec `quality-gates` |
| 4 | Revisions have no server-side precondition. Two writers (canvas and MCP) both read latest = 3 and post; both get stored as 4 and 5. `ControlPlaneFlowStore.saveFlow` detects the race only after writing and throws `RevisionConflictError`, but the loser's content (number 5) is already stored and is now the newest revision, so the next reader sees the flow the losing writer was told was rejected. | medium | API contract | accept `expectedRevision` (or `If-Match`) on `POST /v1/components/{id}/revisions` and return 409 inside the locked transaction; Open Mercato `oss-optimistic-locking` |
| 5 | `GET /v1/components`, `/v1/deployments` and `/v1/runtimes` return every row with no limit; `deployments` grows by one row per deploy forever. With 100k deployments a single list call serialises everything and blocks the event loop for seconds. | medium | API | cursor pagination like `audit.query` (`limit`, `cursor`, `nextCursor`); Open Mercato `list-count-strategies` |
| 6 | `POST /v1/runtimes` is an upsert by client-chosen `id`: a caller with `runtime:register` (admin, owner, the `runtime` role) can overwrite the `environment` and `labels` of an existing runtime, moving a production runtime into "dev" in listings. | medium | API | separate create (409 on existing id) from update; bind runtime registration to the certificate identity |
| 7 | `deleteComponent` in `PgRepository` checks for revisions and then deletes outside a transaction; a revision inserted in between makes the `DELETE` fail on the foreign key (`23503`) and the client gets `500 internal` instead of 409. | low | repository | run the check and delete in one transaction or map `23503` to 409 |
| 8 | A client-supplied `digest` on `POST .../revisions` is stored as is. A caller can store content whose digest does not match, and runtimes receive that digest as `bundle_digest`. | medium | API, deployment | verify the digest when the content is the bundle itself, or keep a separate `artifactDigest` field next to a server-computed `contentDigest`; see `deployment-bundles` |
| 9 | Migrations are forward-only and run automatically at start-up from every replica. Two replicas starting together both try to apply the same file; the second fails on the `schema_migrations` primary key and exits. | low | operations | take a Postgres advisory lock in `migrate()` |
| 10 | Outbox rows are marked published even when the push failed or the runtime is offline, and the table is never pruned. | low | gateway | acceptable while `Hello` reconciles; add retention (delete published rows older than N days) |
| 11 | `labels` on `POST /v1/runtimes` is not validated; a non-object value is stored as JSONB and later breaks clients expecting `Record<string,string>`. | low | validation | validate `labels` as a string map |

## Improvement opportunities

- Generate request/response validation from `openapi.yaml` (or the reverse) so the two cannot drift;
  today only the route list is cross-checked by `policy.test.ts`.
- Return `ETag` on component and revision reads and honour `If-Match` on `PATCH`.
- Add `Location` headers on 201/202 and a `deploymentId` link from desired state.
- Replace `Repository` list methods with filter objects that already carry `workspaceId`, `limit` and
  `cursor`, so isolation and pagination are added in one change.

## References

- Open Mercato: `openapi-route-declared-response-precedence` (declared responses win over handler
  output), `oss-optimistic-locking` (version column + 409), `list-count-strategies` (cursor vs count).
- [ADR 0009](../../adr/0009-outbound-only-runtime-gateway.md).

## Changelog

- 2026-11-22: created as built from https://github.com/nurtuo/nurtuo/pull/26.
