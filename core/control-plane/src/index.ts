// SPDX-License-Identifier: AGPL-3.0-only
// Control plane entry point. Services: component repo (Postgres), packaging, deploy and desired
// state, runtime gateway (Connect/gRPC, runtimes dial out), telemetry ingest, public API, MCP host.
// Shipped as one executable via `bun build --compile` (Node.js as fallback runtime).
import { MCP_TOOLS_V1 } from "@nurtuo/mcp";

console.log(`nurtuo-control-plane: not implemented yet (MCP tools planned: ${MCP_TOOLS_V1.join(", ")})`);
