// SPDX-License-Identifier: AGPL-3.0-only
import { createHash } from "node:crypto";
import { createServer, type Server } from "node:http";
import { ApiError, type ComponentKind, type DeploymentStatus } from "./domain.js";
import type { Repository } from "./repository.js";
import type { AuthHook } from "./auth/middleware.js";
import { matchPolicy } from "./auth/policy.js";

export interface ApiRequest { method: string; path: string; query?: Record<string, string>; body?: unknown; /** lower-cased header names */ headers?: Record<string, string | undefined>; /** peer address */ ip?: string; /** aborted when the client disconnects before the response is sent */ signal?: AbortSignal }
export interface ApiResponse { status: number; body?: unknown; headers?: Record<string, string | string[]> }

/**
 * Auth integration point (see ./auth). `auth: "disabled"` is DEV/TEST ONLY: every route is open to
 * anyone who can reach the socket. main.ts refuses it unless NURTUO_INSECURE_NO_AUTH=1.
 */
export interface HandlerOptions { auth: "disabled" | AuthHook }

const KINDS: ComponentKind[] = ["flow", "map", "connector", "script"];
const STATUSES: DeploymentStatus[] = ["pending", "running", "failed", "stopped"];

/** Canonical JSON (sorted keys) so equal documents get equal digests. */
export function canonicalJson(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonicalJson).join(",")}]`;
  if (v && typeof v === "object") {
    const o = v as Record<string, unknown>;
    return `{${Object.keys(o).sort().map((k) => `${JSON.stringify(k)}:${canonicalJson(o[k])}`).join(",")}}`;
  }
  return JSON.stringify(v);
}
export const digestOf = (content: unknown) => `sha256:${createHash("sha256").update(canonicalJson(content)).digest("hex")}`;

const bad = (msg: string) => new ApiError(400, "bad_request", msg);
const notFound = (what: string) => new ApiError(404, "not_found", `${what} not found`);
function obj(b: unknown): Record<string, unknown> {
  if (!b || typeof b !== "object" || Array.isArray(b)) throw bad("body must be a JSON object");
  return b as Record<string, unknown>;
}
function str(o: Record<string, unknown>, k: string, required = true): string | undefined {
  const v = o[k];
  if (v === undefined && !required) return undefined;
  if (typeof v !== "string" || v === "") throw bad(`${k} must be a non-empty string`);
  return v;
}

/** Transport-agnostic handler; testable without HTTP or a database. */
export function createHandler(repo: Repository, opts: HandlerOptions = { auth: "disabled" /* dev/test default; see HandlerOptions */ }) {
  const hook = opts.auth === "disabled" ? undefined : opts.auth;
  return async function handle(req: ApiRequest): Promise<ApiResponse> {
    try { return hook ? await hook.handle(req, (r) => routeSafe(repo, r)) : await route(repo, req); }
    catch (e) {
      if (e instanceof ApiError) return { status: e.status, body: { error: { code: e.code, message: e.message } } };
      return { status: 500, body: { error: { code: "internal", message: "internal error" } } };
    }
  };
}

/** route() with ApiError mapped to a response, for use as the auth hook's `next`. */
async function routeSafe(repo: Repository, req: ApiRequest): Promise<ApiResponse> {
  try { return await route(repo, req); }
  catch (e) { if (e instanceof ApiError) return { status: e.status, body: { error: { code: e.code, message: e.message } } }; throw e; }
}

async function route(repo: Repository, req: ApiRequest): Promise<ApiResponse> {
  const seg = req.path.replace(/\/+$/, "").split("/").filter(Boolean);
  const m = req.method.toUpperCase();
  if (seg[0] !== "v1") throw notFound("route");
  const [, res, id, sub, subId] = seg;

  // Path-scoped workspace routes are served only by the auth module. Without it (NURTUO_INSECURE_NO_AUTH=1)
  // they do not exist: 404, not the 405 the generic fallthrough below would give (canvas-run spec D9).
  if (res === "workspaces" && id) throw notFound("route");

  if (res === "components") {
    if (!id) {
      if (m === "GET") return { status: 200, body: { items: await repo.listComponents() } };
      if (m === "POST") {
        const b = obj(req.body);
        const kind = (str(b, "kind", false) ?? "flow") as ComponentKind;
        if (!KINDS.includes(kind)) throw bad(`kind must be one of ${KINDS.join(", ")}`);
        const name = str(b, "name")!;
        if (!/^[a-z0-9][a-z0-9._-]*$/.test(name)) throw bad("name must match ^[a-z0-9][a-z0-9._-]*$");
        const description = typeof b.description === "string" ? b.description : "";
        return { status: 201, body: await repo.createComponent({ name, kind, description }) };
      }
    } else if (!sub) {
      if (m === "GET") { const c = await repo.getComponent(id); if (!c) throw notFound("component"); return { status: 200, body: c }; }
      if (m === "PATCH") {
        const b = obj(req.body);
        if (b.description !== undefined && typeof b.description !== "string") throw bad("description must be a string");
        const c = await repo.updateComponent(id, { description: b.description as string | undefined });
        if (!c) throw notFound("component");
        return { status: 200, body: c };
      }
      if (m === "DELETE") {
        const r = await repo.deleteComponent(id);
        if (r === "not_found") throw notFound("component");
        if (r === "has_revisions") throw new ApiError(409, "conflict", "component has revisions");
        return { status: 204 };
      }
    } else if (sub === "revisions") {
      if (!subId) {
        if (m === "GET") {
          if (!(await repo.getComponent(id))) throw notFound("component");
          return { status: 200, body: { items: await repo.listRevisions(id) } };
        }
        if (m === "POST") {
          const b = obj(req.body);
          if (!("content" in b) || b.content === null || typeof b.content !== "object") throw bad("content must be a JSON object or array");
          // An externally packaged artifact (e.g. a signed bundle) supplies its own digest.
          const given = str(b, "digest", false);
          if (given !== undefined && !/^sha256:[0-9a-f]{64}$/.test(given)) throw bad("digest must match ^sha256:[0-9a-f]{64}$");
          const r = await repo.createRevision(id, b.content, given ?? digestOf(b.content));
          if (!r) throw notFound("component");
          return { status: 201, body: r };
        }
      } else if (m === "GET") {
        const n = Number(subId);
        if (!Number.isInteger(n) || n < 1) throw bad("revision number must be a positive integer");
        const r = await repo.getRevision(id, n);
        if (!r) throw notFound("revision");
        return { status: 200, body: r };
      }
    }
  }

  if (res === "runtimes") {
    if (!id && m === "GET") return { status: 200, body: { items: await repo.listRuntimes(req.query?.environment) } };
    if (!id && m === "POST") {
      const b = obj(req.body);
      const labels = (b.labels ?? {}) as Record<string, string>;
      return { status: 200, body: await repo.upsertRuntime({ id: str(b, "id")!, name: str(b, "name")!, environment: str(b, "environment")!, version: str(b, "version", false), labels }) };
    }
    if (id && !sub && m === "GET") { const r = await repo.getRuntime(id); if (!r) throw notFound("runtime"); return { status: 200, body: r }; }
    if (id && sub === "deployments" && subId && m === "DELETE") {
      if (!(await repo.getRuntime(id))) throw notFound("runtime");
      const d = await repo.undeploy({ runtimeId: id, componentId: subId });
      if (!d) throw notFound("deployment");
      return { status: 202, body: d };
    }
    if (id && sub === "desired-state" && m === "GET") {
      if (!(await repo.getRuntime(id))) throw notFound("runtime");
      return { status: 200, body: (await repo.getDesiredState(id)) ?? { runtimeId: id, generation: 0, items: {} } };
    }
  }

  if (res === "deployments") {
    if (!id && m === "POST") {
      const b = obj(req.body);
      const componentId = str(b, "componentId")!;
      const runtimeId = str(b, "runtimeId")!;
      let revisionId = str(b, "revisionId", false);
      if (!revisionId) {
        if (typeof b.revision !== "number") throw bad("revisionId or revision (number) required");
        revisionId = (await repo.getRevision(componentId, b.revision))?.id;
        if (!revisionId) throw notFound("revision");
      }
      return { status: 202, body: await repo.deploy({ componentId, revisionId, runtimeId }) };
    }
    if (!id && m === "GET") return { status: 200, body: { items: await repo.listDeployments({ runtimeId: req.query?.runtimeId, componentId: req.query?.componentId }) } };
    if (id && !sub && m === "GET") { const d = await repo.getDeployment(id); if (!d) throw notFound("deployment"); return { status: 200, body: d }; }
    if (id && sub === "status" && m === "PUT") {
      const b = obj(req.body);
      const status = str(b, "status") as DeploymentStatus;
      if (!STATUSES.includes(status)) throw bad(`status must be one of ${STATUSES.join(", ")}`);
      const d = await repo.updateDeploymentStatus(id, status, typeof b.message === "string" ? b.message : "");
      if (!d) throw notFound("deployment");
      return { status: 200, body: d };
    }
  }

  throw new ApiError(res ? 405 : 404, res ? "method_not_allowed" : "not_found", "no such route");
}


/** Thin node:http adapter. */
export function createHttpServer(repo: Repository, opts: HandlerOptions): Server {
  const handle = createHandler(repo, opts);
  return createServer(async (rq, rs) => {
    const url = new URL(rq.url ?? "/", "http://x");
    // Aborted when the client goes away before the response is complete (kills a running test-run child).
    const ac = new AbortController();
    rs.on("close", () => { if (!rs.writableFinished) ac.abort(); });
    const action = matchPolicy(rq.method ?? "GET", url.pathname)?.policy.action;
    const jsonEnvelope = 512;
    const connectionSettings = 6 * (253 + 255 + 4096) + jsonEnvelope;
    const maxBytes = action === "connection.host_key_scan" ? 6 * 253 + jsonEnvelope
      : action === "connection.put" ? 6 * 4 * 64 * 1024 + connectionSettings
      : Infinity;
    const tooLarge = () => { rs.writeHead(413, { "content-type": "application/json", connection: "close" }).end(JSON.stringify({ error: { code: "payload_too_large", message: `request body larger than ${maxBytes} bytes` } })); };
    if (Number(rq.headers["content-length"] ?? 0) > maxBytes) { tooLarge(); return; }
    const chunks: Buffer[] = [];
    let size = 0;
    for await (const c of rq.iterator({ destroyOnReturn: false })) {
      size += (c as Buffer).length;
      if (size > maxBytes) { tooLarge(); return; } // before JSON parsing
      chunks.push(c as Buffer);
    }
    let body: unknown;
    let out: ApiResponse;
    try { if (chunks.length) body = JSON.parse(Buffer.concat(chunks).toString("utf8")); }
    catch { rs.writeHead(400, { "content-type": "application/json" }).end(JSON.stringify({ error: { code: "bad_request", message: "invalid JSON" } })); return; }
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(rq.headers)) if (v !== undefined) headers[k] = Array.isArray(v) ? v.join(", ") : v;
    out = await handle({ method: rq.method ?? "GET", path: url.pathname, query: Object.fromEntries(url.searchParams), body, headers, ip: rq.socket.remoteAddress, signal: ac.signal });
    rs.writeHead(out.status, { "content-type": "application/json", ...out.headers });
    rs.end(out.body === undefined ? undefined : JSON.stringify(out.body));
  });
}
