// SPDX-License-Identifier: AGPL-3.0-only
// Streamable HTTP transport (stateless: one server + transport per request), for running the MCP
// server standalone behind a reverse proxy. Bind to loopback unless you set an auth token.
import { createServer as createHttpServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { timingSafeEqual } from "node:crypto";
import type { AddressInfo } from "node:net";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";

export interface HttpOptions {
  /** Builds a fresh MCP server for each request. */
  createServer: () => McpServer;
  port?: number;
  /** Default 127.0.0.1. */
  host?: string;
  /** Endpoint path, default /mcp. */
  path?: string;
  /** If set, requests must carry `Authorization: Bearer <token>`. */
  authToken?: string;
  /** Max request body, bytes (default 1 MiB). */
  maxBodyBytes?: number;
  log?: (line: string) => void;
}

const LOOPBACK = new Set(["127.0.0.1", "::1", "localhost", "[::1]"]);

function json(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { "content-type": "application/json" }).end(JSON.stringify(body));
}
const rpcError = (res: ServerResponse, status: number, code: number, message: string) =>
  json(res, status, { jsonrpc: "2.0", error: { code, message }, id: null });

function bearerOk(header: string | undefined, token: string): boolean {
  const m = /^Bearer (.+)$/.exec(header ?? "");
  if (!m) return false;
  const a = Buffer.from(m[1] as string);
  const b = Buffer.from(token);
  return a.length === b.length && timingSafeEqual(a, b);
}

async function readBody(req: IncomingMessage, max: number): Promise<unknown> {
  const chunks: Buffer[] = [];
  let n = 0;
  for await (const c of req) {
    n += (c as Buffer).length;
    if (n > max) throw Object.assign(new Error("body too large"), { status: 413 });
    chunks.push(c as Buffer);
  }
  if (n === 0) return undefined;
  try { return JSON.parse(Buffer.concat(chunks).toString("utf8")); }
  catch { throw Object.assign(new Error("invalid JSON"), { status: 400 }); }
}

export function createMcpHttpServer(opts: HttpOptions): Server {
  const path = opts.path ?? "/mcp";
  const host = opts.host ?? "127.0.0.1";
  const maxBody = opts.maxBodyBytes ?? 1024 * 1024;
  const loopbackOnly = LOOPBACK.has(host);

  return createHttpServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? "/", "http://x");
      if (url.pathname === "/healthz") return json(res, 200, { ok: true });
      if (url.pathname !== path) return json(res, 404, { error: "not found" });
      // DNS-rebinding guard for loopback deployments: browsers send Origin, MCP clients do not.
      const origin = req.headers.origin;
      if (loopbackOnly && origin) {
        let h = "";
        try { h = new URL(origin).hostname; } catch { /* invalid origin */ }
        if (!LOOPBACK.has(h) && !LOOPBACK.has(`[${h}]`)) return rpcError(res, 403, -32000, "Forbidden origin");
      }
      if (opts.authToken && !bearerOk(req.headers.authorization, opts.authToken)) {
        res.setHeader("www-authenticate", "Bearer");
        return rpcError(res, 401, -32001, "Unauthorized");
      }
      if (req.method !== "POST") {
        // Stateless mode: no server-initiated streams or sessions to resume.
        res.setHeader("allow", "POST");
        return rpcError(res, 405, -32000, "Method not allowed");
      }
      const body = await readBody(req, maxBody);
      const server = opts.createServer();
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
      res.on("close", () => { void transport.close(); void server.close(); });
      await server.connect(transport);
      await transport.handleRequest(req, res, body);
    } catch (e) {
      const status = (e as { status?: number }).status ?? 500;
      opts.log?.(`http error: ${(e as Error).message}`);
      if (!res.headersSent) rpcError(res, status, status === 500 ? -32603 : -32600, status === 500 ? "Internal error" : (e as Error).message);
    }
  });
}

export async function startHttp(opts: HttpOptions): Promise<{ server: Server; url: string; close(): Promise<void> }> {
  const server = createMcpHttpServer(opts);
  const host = opts.host ?? "127.0.0.1";
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(opts.port ?? 3333, host, resolve);
  });
  const { port } = server.address() as AddressInfo;
  const shown = host.includes(":") ? `[${host}]` : host;
  return {
    server,
    url: `http://${shown}:${port}${opts.path ?? "/mcp"}`,
    close: () => new Promise<void>((resolve) => { server.close(() => resolve()); server.closeAllConnections?.(); }),
  };
}
