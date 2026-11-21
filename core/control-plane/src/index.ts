// SPDX-License-Identifier: AGPL-3.0-only
// Control plane entry point. Services: component repo (Postgres), packaging, deploy and desired
// state, runtime gateway (Connect/gRPC, runtimes dial out), telemetry ingest, public API, MCP host.
// Shipped as one executable via `bun build --compile` (Node.js as fallback runtime).
export * from "./domain.js";
export type { Repository } from "./repository.js";
export { MemoryRepository } from "./memory-repository.js";
export { PgRepository, migrate, type Db } from "./pg-repository.js";
export { createHandler, createHttpServer, digestOf, canonicalJson, type HandlerOptions } from "./api.js";
export { Gateway, createGatewayServer, runtimeIdFromCert, type GatewayOptions } from "./gateway.js";
export * as auth from "./auth/index.js";
