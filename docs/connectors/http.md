<!-- SPDX-License-Identifier: Apache-2.0 -->
# HTTP connector (`http`)

Native Go connector for REST calls, with OpenAPI 3 import for browsing. Source:
`connectors/http`, descriptor `connectors/http/connector.yaml`, connection schema
`connectors/http/schemas/connection.json`. Version `0.1.0`, host `go-native`.

Status: tested against a local test server in the connector tests and registered in the `nurtuo` binary, so a deployed flow can use it through a bundle connection (secrets resolved from the runtime secret store, see [operations.md](../operations.md)). Not run against a production deployment.

## Operations

All operations are `EXECUTE` with the HTTP method as the custom type, and each declares
`supportsBrowse: true`: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`.

In a flow, `config.operation` may be the method name (`"POST"`), or `GET`, `CREATE`, `UPDATE`,
`UPSERT`, `DELETE`, `QUERY`: the engine sends the generic types and the connector maps
`GET`/`QUERY` to GET, `CREATE` to POST, `UPDATE`/`UPSERT` to PUT, `DELETE` to DELETE.
Each input document is one request. The method can be overridden per document with the property
`http.method`; any other method than the five above is an `APPLICATION_ERROR`.

Per-document request properties:

| Property | Meaning |
|---|---|
| `http.method` | overrides the method |
| `http.path` | path relative to `baseUrl` (otherwise step option `path`) |
| `http.query.<name>` | query parameter |
| `http.header.<name>` | request header |

The document payload is the request body (not sent for GET). Step options (`ExecuteStart.options`):
`path`, `query{}`, `headers{}`, `bodyTemplate`, `retries`, `retryBackoffMs`. `{{name}}`
placeholders in the path (URL-path-escaped), header values and `bodyTemplate` are filled from
document properties; `{{data}}` in `bodyTemplate` is the payload. An unknown placeholder is an
`APPLICATION_ERROR`. Without a `Content-Type` header the connector sets `application/json` when
the body starts with `{` or `[`, else `application/octet-stream`.

The response body becomes an output document `<id>.response` carrying properties `http.status` and
`http.url`.

## Connection settings

| Setting | Type | Default | Notes |
|---|---|---|---|
| `baseUrl` | string (required) | | absolute `http` or `https` URL |
| `auth.type` | `none`, `basic`, `bearer`, `apikey` | `none` | |
| `auth.username`, `auth.password` | string | | basic; username required |
| `auth.token` | string | | bearer; required |
| `auth.apiKeyName`, `auth.apiKeyValue`, `auth.apiKeyIn` | string; `header` or `query` | `header` | apikey; name and value required |
| `headers` | object of strings | | sent on every request |
| `timeoutMs` | integer >= 1 | 30000 | per request attempt |
| `insecureSkipVerify` | boolean | false | disables TLS verification; development only |
| `testPath` | string | `/` | path requested by Test |
| `retries` | integer >= 0 | 2 | see below |
| `retryBackoffMs` | integer >= 0 | 200 | exponential, capped at 5 minutes |
| `openApiSpec` | string | | inline OpenAPI 3 JSON, used by Browse |
| `openApiUrl` | string | | URL of an OpenAPI 3 JSON document, used by Browse |

`password`, `token` and `apiKeyValue` are marked `x-nurtuo-secret` in the JSON schema; the runtime
is expected to inject them from secrets (`secret://name` references in the bundle connection).
`openApiSpec` and `openApiUrl` are declared in `schemas/connection.json` and in the schema returned
by `Describe`. The schema returned by `Describe` is still a reduced copy of the file (it lacks
`headers`, `testPath` and the retry settings).

## Tracked properties

`status_code` and `url` (the engine exposes them as `tracked.status_code` and `tracked.url`). The
URL is recorded with the userinfo password and the API-key query parameter masked.

## Status mapping

| Outcome | Document status |
|---|---|
| 2xx | `SUCCESS` |
| 4xx except 408 and 429 | `APPLICATION_ERROR` (message holds the first 200 bytes of the body) |
| 3xx that was not followed | `APPLICATION_ERROR` |
| 5xx, 408, 429 after retries, transport error, timeout | `FAILURE` |
| response body over 32 MiB | `FAILURE` (never a truncated success) |
| bad path or header placeholder, bad method, invalid request | `APPLICATION_ERROR` |
| invalid connection settings | `FAILURE` for every document |

Retries apply to network errors and HTTP 408, 429, 502, 503, 504, with exponential backoff.
`POST` and `PATCH` are not retried unless `retries` is set explicitly in the connection or in the
step options.

## Test connection

`GET testPath`. Any HTTP response below 500 other than 401/403 is reported as reachable (even
404); 401 and 403 report "authentication rejected"; 5xx reports a server error. The diagnostic
redacts configured secrets.

## Browse (OpenAPI import)

With `openApiSpec` or `openApiUrl` set, `Browse` without an object type id lists the operations
(object type id = `operationId`, or `"<METHOD> <path>"` when absent); with an id it returns the
request and response JSON Schema profiles. Local `#/components/...` references are inlined one
level deep. Without either setting Browse returns not implemented.

## Security notes

- **SSRF.** `baseUrl` and `openApiUrl` are operator supplied and are fetched from wherever the
  runtime runs, including internal networks and cloud metadata endpoints. Only users who may
  configure connections should be able to set them; restrict egress at the network layer. The
  connector does not block private address ranges.
- Credentials: redirects are capped at 10; on a redirect to a different host the connector drops
  the configured headers, the API key and the `Authorization` header.
- `insecureSkipVerify` turns off certificate verification for that connection. Do not use it in
  production.
- Secrets are redacted from error messages and diagnostics. Documents that carry secrets in
  headers or bodies are not scrubbed.

## HTTP listener (Start step)

Inbound HTTP is not handled by this connector but by the runtime's HTTP listener
(`core/runtime/internal/listener`), configured on the flow's `start` step with
`"type": "listener", "connector": "http"`. The listen address is runtime-wide (`http_addr` in
`runtime.yaml`, default `127.0.0.1:0`); `path` and `method` are per flow. See
[flow-reference.md](../flow-reference.md#start) for the schema.

Protection is opt-in and configured on the same `start` step:

```json
{
  "type": "listener",
  "connector": "http",
  "path": "/hooks/github",
  "auth": {
    "type": "hmac",
    "secret": "secret://github-webhook",
    "header": "X-Hub-Signature-256",
    "prefix": "sha256="
  },
  "tls": { "cert": "secret://listener-cert", "key": "secret://listener-key" }
}
```

| `auth.type` | The request must carry | Other fields |
|---|---|---|
| `bearer` | `Authorization: Bearer <secret>` (scheme case-insensitive) | |
| `apiKey` | the secret as the value of `header` | `header`, default `X-API-Key` |
| `hmac` | `prefix` + the HMAC of the raw request body, keyed with the secret, in `header` | `header` (default `X-Signature`), `algorithm` `sha256` (default) or `sha512`, `encoding` `hex` (default) or `base64`, `prefix` (default none) |

- `secret`, `tls.cert` and `tls.key` must be `secret://<name>` references (the schema rejects literal
  values). The runtime resolves them from its secret store (`secrets` in `runtime.yaml`, see
  [operations.md](../operations.md)) when it starts the listener, and adds the values to the log
  redactor. A missing secret fails the listener start, naming the secret.
- Credentials are compared in constant time (`bearer`/`apiKey`: SHA-256 of both sides compared with
  `crypto/subtle`; `hmac`: `hmac.Equal`). A request that fails the check gets `401 Unauthorized`
  (with `WWW-Authenticate: Bearer` for `bearer`) and the flow does not run. `bearer` and `apiKey`
  are checked before the body is read; `hmac` reads the body (still capped at 1 MiB) first.
- For `bearer` and `apiKey` the credential header is **not** copied into the document properties
  (`http.request.header.*`), so it does not reach flow steps, logs or tracked values. The `hmac`
  signature header is not a secret and stays visible.
- `tls` serves HTTPS with the given PEM certificate chain and private key; TLS 1.2 is the minimum.
  Client certificates (mTLS) are not supported.
- **Non-loopback binds need `auth`.** When `http_addr` is not a loopback address (`0.0.0.0`, `::`,
  an empty host such as `:8080`, or any interface address), the runtime refuses to start an HTTP
  listener whose `start` step has no `auth`, unless the step sets `"insecure": true`. Use
  `insecure` only behind a proxy that authenticates for you.
- Secrets are read when the listener starts. A listener restarts only when its deployment's bundle
  changes, so after rotating a secret, redeploy the flow.
- Not covered: rate limiting, replay protection (`hmac` signs only the body, with no timestamp or
  nonce), client certificates, per-flow bind addresses.

## Limits

32 MiB response body per document; 16 MiB OpenAPI document; 10 redirects; one document buffered
at a time; default per-attempt timeout 30 s.
