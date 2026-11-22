<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: HTTP / REST connector

Status: implemented. Date: 2026-11-21.

## TLDR

`connectors/http` is the native Go connector for calling REST/HTTP APIs: one HTTP request per input
document, the HTTP method as the `EXECUTE` custom type, the response body as an output document, and
`Browse` backed by an imported OpenAPI 3 (JSON) document. The one thing to know: `baseUrl`,
`openApiUrl` and redirects are fetched from wherever the runtime runs, and the connector has **no
SSRF filter**; egress control is the operator's job.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/28 (squash commit `7e8f57c`); https://github.com/nurtuo/nurtuo/pull/41 (open, renames the proto message `Descriptor` to `ConnectorDescriptor`, touches `Describe`) |
| ADRs | [ADR 0005](../../adr/0005-set-at-a-time-engine-with-per-document-status.md) (per-document status), [ADR 0010](../../adr/0010-connections-and-secrets.md) (connections and secrets) |
| Issues | none |
| Code | `connectors/http/` (`http.go`, `openapi.go`, `connector.yaml`, `schemas/connection.json`); registered in `core/runtime/internal/connhost/registry.go` (`Builtins`) |
| User docs | [docs/connectors/http.md](../../connectors/http.md) |

## Problem and goals

Most integrations start or end at a REST API. Flow authors need a generic HTTP client step that maps
cleanly onto nurtuo's set-at-a-time model (one document in, one request, one result) and that lets
the canvas offer operations and JSON Schemas from an API's OpenAPI document.

- Goals: GET/POST/PUT/PATCH/DELETE against a configured `baseUrl`; per-document path, query and
  headers; templated bodies; none/basic/bearer/API-key auth injected from secrets; safe retries;
  deterministic per-document status; OpenAPI 3 import for `Browse`; never record credentials.
- Non-goals (M1): OAuth2 flows, pagination, HEAD/OPTIONS, multipart uploads, YAML OpenAPI documents,
  acting as an HTTP *server* (that is the runtime's HTTP listener, `core/runtime/internal/listener`,
  not this connector), SSRF filtering.

## Design (as built)

**Descriptor.** `(*Connector).Describe` in `connectors/http/http.go` returns id `http`, version
`0.1.0` (`const Version`), host `HOST_GO_NATIVE`, display name `HTTP / REST`, five `EXECUTE`
operations with custom types `GET`, `POST`, `PUT`, `PATCH`, `DELETE` (each `SupportsBrowse: true`),
tracked properties `status_code` and `url`, and `Capabilities{Streaming: true}`. `connector.yaml`
mirrors it for the catalogue. The connection schema returned by `Describe` is the Go map
`connectionSchema`, a **reduced copy** of `schemas/connection.json` (see Gaps).

**Connection settings** (`parseConfig`, `schemas/connection.json`):

| Key | Default | Notes |
|---|---|---|
| `baseUrl` (required) | | absolute `http`/`https` URL with a host, else invalid |
| `auth.type` | `none` | `none`, `basic` (needs `username`), `bearer` (needs `token`), `apikey` (needs `apiKeyName`, `apiKeyValue`) |
| `auth.username`, `auth.password`, `auth.token`, `auth.apiKeyName`, `auth.apiKeyValue`, `auth.apiKeyIn` | `apiKeyIn: header` | `password`, `token`, `apiKeyValue` are `x-nurtuo-secret: true` |
| `headers` | `{}` | sent on every request |
| `timeoutMs` | 30000 | per attempt (`context.WithTimeout` around each `client.Do`) |
| `insecureSkipVerify` | false | `insecureTLS()` in `openapi.go` |
| `testPath` | `/` | used by `Test` |
| `retries`, `retryBackoffMs` | 2, 200 | `retriesSet` remembers an explicit value |
| `openApiSpec`, `openApiUrl` | | inline JSON wins over the URL |

**Execute** (`(*Connector).Execute`). The first message must be `ExecuteStart`; the connection is
parsed once (`cfgErr`), step options once (`parseOptions`: `path`, `query{}`, `headers{}`,
`bodyTemplate`, `retries`, `retryBackoffMs`). Chunks are accumulated per document in a
`bytes.Buffer`; a document is flushed when its `Last` chunk arrives, when a chunk of a different
document arrives, on `ExecuteEnd`, or on EOF. Only one document is buffered at a time.

Per document (`doDocument`):

1. Method: property `http.method`, else `ExecuteStart.custom_type`, else mapped from the generic
   operation type in `methodFor` (`GET`/`QUERY` → GET, `CREATE` → POST, `UPDATE`/`UPSERT` → PUT,
   `DELETE` → DELETE). Anything outside `methods` is `APPLICATION_ERROR`.
2. Path: property `http.path`, else option `path`; `{{name}}` placeholders are filled from document
   properties by `render` with `url.PathEscape`. Header values (option `headers`) and `bodyTemplate`
   are rendered without escaping; `{{data}}` is the payload. An unknown placeholder is
   `APPLICATION_ERROR`.
3. Query: option `query`, then properties `http.query.<name>`; headers: option `headers`, then
   properties `http.header.<name>`. `buildRequest` applies connection `headers`, then per-request
   headers, then the auth header, so a document cannot override `Authorization` for basic/bearer.
4. Body: rendered `bodyTemplate`, else the payload; GET never sends a body. `Content-Type` defaults
   via `sniff` (`application/json` when the body starts with `{` or `[`, else
   `application/octet-stream`).
5. Retries: network errors and `retryable` codes (408, 429, 502, 503, 504) are retried up to
   `retries` times with `backoffFor` (base `<<` attempt, capped at `maxBackoff` = 5 minutes). POST
   and PATCH get `retries = 0` unless `retries` is set explicitly in the connection
   (`retriesSet`) or the step options (`hasRetryOption`).
6. Response: read through `io.LimitReader(maxResponseBody+1)`; `maxResponseBody` is 32 MiB. The
   body is emitted as output document `<id>.response` in 64 KiB chunks by `sendOutput`, with
   properties `http.status` and `http.url`. Tracked: `status_code`, `url` (via `safeURL`).

**Status mapping** (`finish`, `doDocument`):

| Outcome | Status |
|---|---|
| 2xx | `SUCCESS` |
| 4xx except 408/429; a 3xx that was not followed | `APPLICATION_ERROR`, message `HTTP <code>: <first 200 bytes>` (`snippet`) |
| 5xx; 408/429 after retries; transport error; timeout; cancellation | `FAILURE` |
| body over 32 MiB | `FAILURE` ("exceeds the ... byte limit; not recorded"), never a truncated `SUCCESS` |
| bad method, unknown placeholder, invalid request | `APPLICATION_ERROR` |
| invalid connection settings | `FAILURE` for every document (the stream itself stays OK) |

**Redirects** (`checkRedirect`): at most `maxRedirects` = 10; when the redirect target host differs
from the first request's host, the configured `headers`, the API-key header or query parameter and
`Authorization` are removed.

**Test** (`(*Connector).Test`): `GET testPath`. 401/403 → `Ok: false` "authentication rejected";
5xx → "server error"; any other response (even 404) → reachable. Errors go through `redact`.

**Browse** (`openapi.go`): without `openApiSpec`/`openApiUrl` it returns
`connector.ErrNotImplemented`. Otherwise `loadSpec` (16 MiB cap, HTTP 200 required) and
`ParseOpenAPI` (JSON only, `openapi: 3.x`) produce `OperationInfo` items. Without
`object_type_id`, Browse lists object types (id = `operationId` or `"<METHOD> <path>"`, label =
summary); with one it returns `RequestProfile`/`ResponseProfile` as `FORMAT_JSON_SCHEMA` (the
`application/json` request body and the lowest 2xx `application/json` response, local `$ref`s
resolved by `resolve`, up to 8 hops). `BrowseRequest.custom_type` filters by method.

## Contracts

| Contract | Stability |
|---|---|
| Connector id `http`, version `0.1.0`, operations `EXECUTE` × {GET, POST, PUT, PATCH, DELETE} | experimental |
| Connection keys in `schemas/connection.json` | experimental |
| Step options `path`, `query`, `headers`, `bodyTemplate`, `retries`, `retryBackoffMs` | experimental |
| Document properties `http.method`, `http.path`, `http.query.<name>`, `http.header.<name>` | experimental |
| Output document id `<id>.response` with properties `http.status`, `http.url` | experimental |
| Tracked properties `status_code`, `url` | experimental |
| `ParseOpenAPI` / `OperationInfo` (exported Go API) | internal |
| `Connector.Client` override (tests) | internal |

## Compatibility

No contract change in this spec. Open https://github.com/nurtuo/nurtuo/pull/41 changes the return
type of `Describe` to `*connectorv1.ConnectorDescriptor` (the `connector.Connector` interface in
`sdk/go/connector/connector.go` changes with it); field numbers stay, so the wire format is
unchanged, but Go code that names `connectorv1.Descriptor` must be renamed. This spec must be updated
when PR 41 merges.

## Security

- **Secrets.** `password`, `token`, `apiKeyValue` are `x-nurtuo-secret`; the runtime resolves them
  (`core/runtime/internal/connections`, `SecretPaths`) and wraps the connector in `redacting`
  (`core/runtime/internal/connhost/redacting.go`) as a second layer. In the connector, `redact`
  replaces exact secret values in error messages, snippets and Test diagnostics; `safeURL` masks the
  userinfo password and the API-key query parameter in the tracked `url`.
- **Redirects.** Cross-host redirects drop configured credentials and headers (see Design); chains
  are capped at 10.
- **TLS.** Certificate verification is on by default; `insecureSkipVerify` is an explicit
  per-connection opt-in.
- **Limits.** 32 MiB per response, 16 MiB per OpenAPI document, 1 MiB drained by Test.
- **Idempotency.** POST/PATCH are not retried by default.
- **SSRF.** Not filtered. See Gaps 1.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestContract` in `connectors/http/http_test.go` | shared suite (`contracttest.Run`): descriptor sanity, Test redaction of the bearer token, Execute status mapping (`ok` 200, `missing` 404, `boom` 500, `flaky` 503→503→200 with 2 retries, `nopath` unknown placeholder) | `cd connectors && GOWORK=off go test -race ./http/...` | asserts the flaky endpoint was hit 3 times |
| `TestMethodsBodyAndOutput` | GET/PUT/PATCH/DELETE, `http.path`/`http.query.*`/`http.header.*`, GET drops the body, `<id>.response` output, tracked `status_code` | same | |
| `TestAuthRejected` | Test reports 401 as not OK | same | |
| `TestAPIKeyInQuery` | API key sent as query parameter | same | |
| `TestParseOpenAPI` | operation ids, `$ref` inlining, request/response schemas | same | |
| `TestAPIKeyInQueryNotLeakedInTracked` in `findings_test.go` | tracked `url` masks the API key | same | |
| `TestCrossHostRedirectStripsCredentials` | API key, custom header and `Authorization` not forwarded cross-host | same | |
| `TestRedirectLoopCapped` | redirect loop ends, not `SUCCESS`, at most 11 hits | same | |
| `TestOversizeResponseNotSuccess` | 33 MiB body is not `SUCCESS` | same | |
| `TestConnectionSchemaDeclaresOpenAPISettings`, `TestDescribeSchemaDeclaresOpenAPISettings` in `schema_test.go` | `openApiSpec`/`openApiUrl` present in both schemas | same | |

`stream_test.go` holds the in-memory `memStream` used by the direct tests. All tests use
`httptest` servers; nothing runs against a real API. Contract-suite subtests
`ExecuteLargePayload`, `CrashBehaviour` and `Listen` are skipped by
`sdk/go/connector/contracttest` for every connector; `Browse` is not part of the suite.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A flow author sets `baseUrl` (or a public API answers with a redirect) to `http://169.254.169.254/latest/meta-data/iam/...`; the runtime fetches cloud credentials and stores them as the `<id>.response` document, visible in traces. `schemas/connection.json` even claims `openApiUrl` is "subject to the same SSRF policy as baseUrl", but no policy exists. | high | security | Add an opt-out egress guard (deny link-local, loopback, RFC 1918 by default, checked in a `net.Dialer.Control` so redirects and DNS rebinding are covered); fix the schema text; until then restrict egress at the network layer (documented in `docs/connectors/http.md`). |
| 2 | `baseUrl` is `https://api.example.com`; the API redirects to `http://api.example.com/...` (same host, scheme downgrade). `checkRedirect` only compares hosts, so `Authorization`, the API key and custom headers are sent in clear text. | medium | security | Refuse `https` → `http` redirects, or strip credentials on any scheme change; add a test next to `TestCrossHostRedirectStripsCredentials`. |
| 3 | A 2 GB document is POSTed: `Execute` accumulates it in `document.data` (`bytes.Buffer`) with no cap while the descriptor advertises `Streaming: true`; the runtime process is OOM-killed. | medium | reliability | Stream the request body (`io.Pipe` fed by chunks, retries only when the body is replayable) or enforce a request-size cap with an `APPLICATION_ERROR`. |
| 4 | A rate-limited API returns `429` with `Retry-After: 30`; the connector ignores the header and retries after 200 ms and 400 ms, then reports `FAILURE`. Conversely, a step option `retries: 1000` keeps one document retrying for hours (backoff capped at 5 minutes, retries uncapped) and blocks the batch. | medium | reliability | Honour `Retry-After` (bounded), cap `retries` in `parseOptions`/`parseConfig` and in the schema. |
| 5 | The canvas builds a connection form from `Describe`; `connectionSchema` lacks `headers`, `insecureSkipVerify`, `testPath`, `retryBackoffMs`, `auth.username`, `auth.apiKeyName`, `auth.apiKeyIn` and all defaults, so these settings cannot be entered there although `schemas/connection.json` has them. No test compares the two schemas or the descriptor with `connector.yaml` (no connector sets `contracttest.Options.Golden`). | low | contract | Embed `schemas/connection.json` with `//go:embed` as the SFTP connector does; add a golden descriptor test. |
| 6 | A debugging endpoint echoes request headers (`Authorization: Basic dTpwdw==`) in its response body; `redact` only matches the raw secret, so the base64 credential ends up in the output document and in the `APPLICATION_ERROR` snippet. | low | security | Also redact the encoded `Basic` value; document that response bodies are not scrubbed (already in user docs). |
| 7 | An API publishes its OpenAPI document as YAML (most do); `ParseOpenAPI` fails with "only JSON documents are supported" and `Browse` offers nothing. `Browse` is not covered by the contract suite. | low | usability | Accept YAML; add a Browse case to `contracttest`. |
| 8 | `insecureSkipVerify: true` copied from a dev bundle into production silently disables TLS verification; nothing warns. | low | security | Surface it in the Test diagnostic and in `Manager.Describe` output; optionally refuse it when the runtime runs in production mode. |

## Improvement opportunities

- **OAuth2** client-credentials and refresh-token grants with token caching per connection (today
  only static bearer tokens); mTLS client certificates.
- **Pagination**: link-header, cursor and offset strategies that emit one document per page or per
  item, with a page cap.
- HEAD and OPTIONS methods; multipart/form-data and `application/x-www-form-urlencoded` bodies.
- Retry on 500 as an opt-in; jitter in `backoffFor`.
- Response-to-JSON helpers: emit one document per array element of the response.
- OpenAPI: YAML input, servers/security schemes to prefill `baseUrl` and `auth`, path parameters
  surfaced as properties.
- Request/response size and timing as tracked properties (`duration_ms`, `bytes`).

## References

- Open Mercato `payment-gateway-webhook-transport-hooks`: transport hooks around outbound HTTP; an
  idea for pluggable auth/signing in `buildRequest`.
- Open Mercato `inbound-webhook-handlers`: the inbound side; here it belongs to the runtime HTTP
  listener, not this connector.

## Changelog

- 2026-11-22: created (as built, from `main` at `3d3d506`).
