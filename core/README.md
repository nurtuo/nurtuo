# core/ (AGPL-3.0-only)

Server-side Nurtuo. Everything here is AGPL-3.0; it may depend on the Apache-2.0 directories,
never the other way round.

| Dir | What |
|---|---|
| `runtime/` | Go module: `nurtuo` binary (supervisor, engine, embedded NATS JetStream, listeners, bridge host, WASM scripts, service install) |
| `control-plane/` | TypeScript: API/BFF, component repo, packaging, deploy, runtime gateway, telemetry ingest, MCP host |
| `web/` | TypeScript: canvas UI (React Flow + elkjs, CommandBus) |
| `mcp/` | TypeScript: MCP server tools |
| `ai/` | TypeScript: AI gateway, generation, mapping suggestions, evals |
| `deploy/` | systemd / Windows service / launchd templates, MSI/deb/rpm/pkg packaging, Helm chart, dev stack |
