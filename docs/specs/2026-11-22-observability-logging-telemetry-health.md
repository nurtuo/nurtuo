<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Observability: logging, telemetry and health

Status: draft. Date: 2026-11-22.

## TLDR

Today the runtime logs `slog` text to stderr, ignores its own `log_level` key, has an empty
`internal/telemetry` package and no health endpoint; the control plane logs with `console.*` and has
no health endpoint either. This spec defines structured JSON logs with a shared field set,
OpenTelemetry traces and metrics for flow executions, `/livez` and `/readyz` on both processes, and
an error-reporting policy, all off-network by default.

## Status and links

| | |
|---|---|
| Pull requests | none yet |
| ADRs | to write: "OpenTelemetry as the only telemetry API" |
| Code (today) | `core/runtime/internal/telemetry/doc.go` (package comment only), `core/runtime/cmd/nurtuo/run.go` and `service.go` (`slog.NewTextHandler`), `core/runtime/internal/runtime/config.go` (`LogLevel`, unused), `core/runtime/internal/secrets/redact.go` (redacting `slog` handler), `core/control-plane/src/main.ts` (`console.log`/`warn`/`error`), `core/runtime/cmd/nurtuo/standalone_supervisor.go` (`HTTPProbe`), `core/mcp/src/http.ts` (`GET /healthz`) |
| User docs | [operations.md](../operations.md) "Logs" (states that `log_level` is ignored) |

## Problem and goals

An operator running `nurtuo service` and `nurtuo-control-plane` cannot answer "is it up", "is it
ready to take traffic", "which flow run failed and why" or "how many documents per second" without
reading text logs. Kubernetes probes and load balancers have nothing to call. The architecture
promises "telemetry to Postgres (standalone)" (ARCHITECTURE.md §11, epic 8) and a telemetry package
that "buffers execution summaries and OpenTelemetry spans"; neither exists.

- Goals:
  1. Structured logs: JSON by default for services, text for terminals; levels honoured; one field
     vocabulary across Go and TypeScript.
  2. Health: liveness and readiness endpoints on runtime and control plane with documented checks.
  3. Traces and metrics through OpenTelemetry (OTLP export), disabled unless configured.
  4. Execution summaries per flow run, stored by the control plane (standalone: PostgreSQL).
  5. Error reporting: no third-party crash reporter by default; panics and unhandled rejections are
     logged with a stack and a correlation id.
- Non-goals: a reporting UI (separate spec), log shipping agents, vendor-specific exporters,
  ClickHouse (`--profile reporting` in ARCHITECTURE.md §8).

## Design (proposed)

### Logging

- Runtime: one constructor `telemetry.NewLogger(cfg)` replaces the three `slog.New(...)` calls.
  `log_level` (`debug|info|warn|error`, default `info`) and a new `log_format` (`json|text`, default
  `json` under a service manager, `text` on a TTY) are read from the runtime config. The redacting
  handler from `internal/secrets` stays the outermost handler.
- Control plane: a small logger module (`core/control-plane/src/log.ts`) writing JSON lines to
  stdout; `NURTUO_LOG_LEVEL`, `NURTUO_LOG_FORMAT`. `console.*` is removed from `src/` (lint rule
  once ESLint exists, see [quality-gates](2026-11-22-quality-gates.md)).
- Common fields: `time`, `level`, `msg`, `component` (`runtime|control-plane|gateway|mcp`),
  `runtime_id`, `deployment_id`, `flow_id`, `revision`, `execution_id`, `step_id`, `trace_id`,
  `span_id`, `error.code`, `error.message`. Secrets never appear (existing redaction plus a test).

### Health

| Process | Endpoint | Liveness (`/livez`) | Readiness (`/readyz`) |
|---|---|---|---|
| control plane (REST port) | `GET /livez`, `GET /readyz` | event loop responds | database reachable (`SELECT 1` within 1 s), migrations at the expected version, gateway port listening |
| runtime | `GET /livez`, `GET /readyz` on an opt-in admin listener `admin_addr` (default off, loopback when on) | process responsive | gateway session established at least once or a cached desired state is applied; every deployment in `running` or reported `error` |

Responses: `200 {"status":"ok"}` or `503 {"status":"fail","checks":{"<name>":"<reason>"}}`. No
authentication, no internal details beyond check names. `HTTPProbe` in standalone mode switches to
`/readyz` and treats only `200` as healthy (today any status `< 500` counts, so a `401` is
"healthy").

### Traces and metrics

- OpenTelemetry SDKs (Go `go.opentelemetry.io/otel`, Node `@opentelemetry/api`) with OTLP/HTTP
  export when `OTEL_EXPORTER_OTLP_ENDPOINT` is set; otherwise a no-op provider (zero cost).
- Spans: `flow.execute` (attributes `flow_id`, `revision`, `documents.in`, `documents.out`,
  `documents.error`), one child per step, connector calls as client spans, gateway session as a
  long-lived span with events for reconnects. Trace context propagates through the HTTP listener
  (W3C `traceparent`) and the HTTP connector.
- Metrics (names final in the implementing PR): `nurtuo_executions_total{flow_id,status}`,
  `nurtuo_execution_duration_seconds`, `nurtuo_documents_total{flow_id,status}`,
  `nurtuo_queue_depth{queue}`, `nurtuo_dlq_messages_total{queue}`, `nurtuo_gateway_connected`,
  `nurtuo_http_requests_total{route,status}` (control plane). Optional Prometheus scrape at
  `/metrics` on the admin listener.

### Execution summaries

`internal/telemetry` buffers one summary per execution (ids, start, duration, per-status counts,
first error) in a bounded ring (default 10 000 entries, drop-oldest with a counter) and ships
batches over the gateway stream as a new `ExecutionSummaries` message. The control plane stores them
in an `executions` table with a retention setting (default 7 days) and exposes
`GET /v1/executions?flowId=&status=&since=`.

### Error reporting

Go: `recover` at the top of each execution and of each goroutine started by the supervisor, logged
at `error` with stack and `execution_id`; the process keeps running. Node: `process.on("uncaughtException")`
logs and exits non-zero (the supervisor restarts it); `unhandledRejection` logs at `error`. No
telemetry leaves the machine unless an OTLP endpoint is configured.

## Contracts

| Contract | Stability when shipped |
|---|---|
| Runtime config keys `log_level`, `log_format`, `admin_addr` | stable |
| Env vars `NURTUO_LOG_LEVEL`, `NURTUO_LOG_FORMAT`, standard `OTEL_*` | stable |
| `/livez`, `/readyz` paths and response shape | stable |
| Log field names | stable (dashboards depend on them) |
| Metric names and labels | experimental for one minor release, then stable |
| `gateway.proto` message `ExecutionSummaries` | stable once added (`buf breaking` applies) |
| `GET /v1/executions` | experimental |

## Compatibility

Additive except: the default runtime log format under a service manager changes from text to JSON
(operators parsing text lines with `journalctl` greps must set `log_format: text`). The new proto
message is additive; older control planes ignore it (unknown oneof field), older runtimes never send
it.

## Security

- Health endpoints expose only check names and `ok/fail`; the runtime admin listener binds to
  loopback unless configured otherwise.
- Log redaction stays mandatory; a test feeds a connection secret through every log path.
- Labels and span attributes never contain document payloads; `flow_id` cardinality is bounded by the
  number of deployed flows.
- OTLP export over TLS when the endpoint is `https://`; headers from `OTEL_EXPORTER_OTLP_HEADERS`
  are treated as secrets (not logged).

## Testing (acceptance)

| Test | Acceptance criterion |
|---|---|
| `TestLogLevelHonoured` (runtime) | with `log_level: warn`, an `info` record is not written; with `debug` it is |
| `TestJSONLogFields` (runtime) | a failing execution writes one JSON line with `flow_id`, `execution_id`, `step_id`, `error.code` |
| `log.test.ts` (control plane) | JSON lines, level filter, no secret values from a seeded connection |
| `TestReadyz` (runtime) | `/readyz` is 503 before the first desired state is applied, 200 after |
| `health.test.ts` (control plane) | `/readyz` is 503 with the database down, 200 with it up; `/livez` is 200 in both |
| `TestStandaloneProbeRequires200` | `HTTPProbe` treats 401 and 404 as unhealthy |
| `TestExecutionSummariesShipped` (interop) | after 3 runs the control plane lists 3 executions with correct counts |
| `TestTelemetryBufferBounded` | with the gateway down, 20 000 executions keep memory bounded and increment the drop counter |
| `TestNoOpWithoutOTLP` (perf) | `TestPerfBudget` p99 stays within budget with telemetry compiled in and no endpoint |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Span creation per step on the hot path adds latency and breaks the pickup p99 < 5 ms budget. | high | performance | No-op provider by default; sample at the execution level; `TestNoOpWithoutOTLP`. |
| 2 | A runtime offline for a day buffers summaries without bound and is OOM-killed. | high | runtime | Bounded ring with drop counter (above). |
| 3 | High-cardinality labels (`execution_id` as a metric label) explode the metrics backend. | medium | metrics | Only bounded labels on metrics; ids go to spans and logs. |
| 4 | Readiness that depends on the gateway makes every runtime "not ready" during a control plane outage, and an orchestrator restarts healthy runtimes that are still serving their last state. | medium | health | Readiness passes when a cached desired state is applied, even without a session (above). |

## Improvement opportunities

- A `nurtuo doctor` command that calls `/readyz` and prints the failing checks.
- Grafana dashboard JSON in `core/deploy/` once metric names are stable.

## References

- Open Mercato: `structured-logging-facade`, `telemetry-and-otel`, `health-monitoring`,
  `error-reporting-policy`.
- Related as-built specs: [e2e-and-interop-testing](implemented/2026-11-21-e2e-and-interop-testing.md);
  `runtime-supervisor-and-deployer` and `performance-budget` (written in parallel).

## Changelog

- 2026-11-22: created as draft.
