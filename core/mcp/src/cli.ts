// SPDX-License-Identifier: AGPL-3.0-only
// Command line of `nurtuo-mcp`. In stdio mode stdout carries the protocol, so every log line
// goes to stderr.
import { parseArgs } from "node:util";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { ControlPlaneFlowStore } from "./control-plane-store.js";
import { startHttp } from "./http.js";
import { createServer, type ServerDeps } from "./server.js";
import { InMemoryDeployer, InMemoryFlowStore } from "./store.js";

export const USAGE = `nurtuo-mcp - Nurtuo MCP server

Usage: nurtuo-mcp [options]

  --store <memory|control-plane>  Flow backend (default: memory, or control-plane when --url is given)
  --url <url>                     Control plane base URL            (env NURTUO_URL)
  --token <token>                 Control plane bearer token        (env NURTUO_TOKEN)
  --transport <stdio|http>        Transport (default: stdio)
  --port <n>                      HTTP port (default 3333)
  --host <addr>                   HTTP bind address (default 127.0.0.1)
  --auth-token <token>            Require this bearer token on the HTTP endpoint (env NURTUO_MCP_TOKEN)
  --allow-deploy                  Expose the deploy tool (default: flows are read/write, no deploy)
  --max-ops <n>                   Max ops per apply_ops call (default 200)
  --timeout-ms <n>                Per-call timeout (default 30000)
  -h, --help                      Show this help
`;

export interface CliConfig {
  store: "memory" | "control-plane";
  url?: string;
  token?: string;
  transport: "stdio" | "http";
  port: number;
  host: string;
  authToken?: string;
  allowDeploy: boolean;
  maxOps?: number;
  timeoutMs?: number;
  help: boolean;
}

export class UsageError extends Error {}

const num = (name: string, v: string | undefined): number | undefined => {
  if (v === undefined) return undefined;
  const n = Number(v);
  if (!Number.isInteger(n) || n < 1) throw new UsageError(`--${name} must be a positive integer`);
  return n;
};

export function parseCli(argv: string[], env: Record<string, string | undefined> = process.env): CliConfig {
  let values;
  try {
    ({ values } = parseArgs({
      args: argv,
      options: {
        store: { type: "string" }, url: { type: "string" }, token: { type: "string" },
        transport: { type: "string" }, port: { type: "string" }, host: { type: "string" },
        "auth-token": { type: "string" }, "allow-deploy": { type: "boolean" },
        "max-ops": { type: "string" }, "timeout-ms": { type: "string" }, help: { type: "boolean", short: "h" },
      },
      strict: true,
    }));
  } catch (e) { throw new UsageError((e as Error).message); }

  const url = values.url ?? env["NURTUO_URL"];
  const store = (values.store ?? (url ? "control-plane" : "memory")) as string;
  if (store !== "memory" && store !== "control-plane") throw new UsageError("--store must be memory or control-plane");
  const transport = (values.transport ?? "stdio") as string;
  if (transport !== "stdio" && transport !== "http") throw new UsageError("--transport must be stdio or http");
  if (store === "control-plane" && !url) throw new UsageError("--store control-plane needs --url (or NURTUO_URL)");
  const token = values.token ?? env["NURTUO_TOKEN"];
  const authToken = values["auth-token"] ?? env["NURTUO_MCP_TOKEN"];
  const host = values.host ?? "127.0.0.1";
  if (transport === "http" && !["127.0.0.1", "::1", "localhost"].includes(host) && !authToken) {
    throw new UsageError("binding the HTTP transport to a non-loopback address requires --auth-token");
  }
  const maxOps = num("max-ops", values["max-ops"]);
  const timeoutMs = num("timeout-ms", values["timeout-ms"]);
  return {
    store, transport, host, port: num("port", values.port) ?? 3333, allowDeploy: values["allow-deploy"] === true, help: values.help === true,
    ...(url ? { url } : {}), ...(token ? { token } : {}), ...(authToken ? { authToken } : {}),
    ...(maxOps ? { maxOps } : {}), ...(timeoutMs ? { timeoutMs } : {}),
  };
}

export function depsFromConfig(cfg: CliConfig): ServerDeps {
  const limits = { ...(cfg.maxOps ? { maxOps: cfg.maxOps } : {}), ...(cfg.timeoutMs ? { callTimeoutMs: cfg.timeoutMs } : {}) };
  if (cfg.store === "control-plane") {
    const store = new ControlPlaneFlowStore({ baseUrl: cfg.url as string, ...(cfg.token ? { token: cfg.token } : {}) });
    return { store, deployer: store, allowDeploy: cfg.allowDeploy, limits };
  }
  const store = new InMemoryFlowStore();
  return { store, deployer: new InMemoryDeployer(store), allowDeploy: cfg.allowDeploy, limits };
}

export async function main(argv: string[], env: Record<string, string | undefined> = process.env): Promise<void> {
  const err = (s: string) => process.stderr.write(`${s}\n`);
  let cfg: CliConfig;
  try { cfg = parseCli(argv, env); }
  catch (e) {
    if (!(e instanceof UsageError)) throw e;
    err(`nurtuo-mcp: ${e.message}\n\n${USAGE}`);
    process.exitCode = 2;
    return;
  }
  if (cfg.help) { process.stdout.write(USAGE); return; }
  const deps = depsFromConfig(cfg);
  const mode = `${cfg.store === "control-plane" ? `control plane ${cfg.url}` : "in-memory store"}${cfg.allowDeploy ? ", deploy enabled" : ""}`;

  if (cfg.transport === "http") {
    const h = await startHttp({
      createServer: () => createServer(deps), port: cfg.port, host: cfg.host, log: err,
      ...(cfg.authToken ? { authToken: cfg.authToken } : {}),
    });
    err(`nurtuo-mcp listening on ${h.url} (${mode})`);
    const stop = () => { void h.close().then(() => process.exit(0)); };
    process.once("SIGINT", stop);
    process.once("SIGTERM", stop);
    return;
  }
  const server = createServer(deps);
  await server.connect(new StdioServerTransport());
  err(`nurtuo-mcp ready on stdio (${mode})`);
}
