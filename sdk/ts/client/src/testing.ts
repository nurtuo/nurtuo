// SPDX-License-Identifier: Apache-2.0
// In-memory fake of the control-plane REST API for tests of API consumers (client, CLI).
// It follows core/control-plane/openapi.yaml but shares no code with the control plane.
import { createHash, randomUUID } from "node:crypto";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";

export interface FakeOptions {
  /** When set, requests without `Authorization: Bearer <token>` get 401. */
  token?: string;
}
export interface FakeControlPlane {
  url: string;
  /** Every request seen, in order. */
  requests: { method: string; path: string; query: Record<string, string>; body?: unknown; authorization?: string }[];
  /** Make the next `count` requests fail with `status` before routing (for retry tests). */
  failNext(count: number, status: number, headers?: Record<string, string>): void;
  /** Make the next request hang until the client gives up. */
  hangNext(): void;
  /** Seed a runtime. */
  addRuntime(r: { id: string; name: string; environment: string; version?: string }): void;
  close(): Promise<void>;
}

const canon = (v: unknown): string => {
  if (Array.isArray(v)) return `[${v.map(canon).join(",")}]`;
  if (v && typeof v === "object") {
    const o = v as Record<string, unknown>;
    return `{${Object.keys(o).sort().map((k) => `${JSON.stringify(k)}:${canon(o[k])}`).join(",")}}`;
  }
  return JSON.stringify(v);
};

type Obj = Record<string, any>;

export async function startFakeControlPlane(options: FakeOptions = {}): Promise<FakeControlPlane> {
  const components = new Map<string, Obj>();
  const revisions = new Map<string, Obj[]>();
  const runtimes = new Map<string, Obj>();
  const desired = new Map<string, Obj>();
  const deployments = new Map<string, Obj>();
  const requests: FakeControlPlane["requests"] = [];
  let failures: { count: number; status: number; headers: Record<string, string> } | undefined;
  let hang = false;

  const server: Server = createServer(async (rq: IncomingMessage, rs) => {
    const url = new URL(rq.url ?? "/", "http://x");
    const chunks: Buffer[] = [];
    for await (const c of rq) chunks.push(c as Buffer);
    let body: unknown;
    const raw = Buffer.concat(chunks).toString("utf8");
    const send = (status: number, b?: unknown, headers: Record<string, string> = {}) => {
      rs.writeHead(status, { "content-type": "application/json", ...headers });
      rs.end(b === undefined ? undefined : JSON.stringify(b));
    };
    const err = (status: number, code: string, message: string) => send(status, { error: { code, message } });
    try { if (raw) body = JSON.parse(raw); } catch { return err(400, "bad_request", "invalid JSON"); }
    const method = rq.method ?? "GET";
    requests.push({ method, path: url.pathname, query: Object.fromEntries(url.searchParams), body, authorization: rq.headers.authorization });

    if (hang) { hang = false; return; }
    if (failures && failures.count > 0) {
      failures.count--;
      return send(failures.status, { error: { code: "injected", message: "injected failure" } }, failures.headers);
    }
    if (options.token && rq.headers.authorization !== `Bearer ${options.token}`) return err(401, "unauthorized", "missing or invalid token");

    const seg = url.pathname.replace(/\/+$/, "").split("/").filter(Boolean);
    const [v, res, id, sub, subId] = seg;
    const b = (body ?? {}) as Obj;
    const now = () => new Date().toISOString();
    if (v !== "v1") return err(404, "not_found", "no such route");

    if (res === "components") {
      if (!id) {
        if (method === "GET") return send(200, { items: [...components.values()] });
        if (method === "POST") {
          if (typeof b.name !== "string" || !/^[a-z0-9][a-z0-9._-]*$/.test(b.name)) return err(400, "bad_request", "name must match ^[a-z0-9][a-z0-9._-]*$");
          const kind = b.kind ?? "flow";
          if (!["flow", "map", "connector", "script"].includes(kind)) return err(400, "bad_request", "invalid kind");
          if ([...components.values()].some((c) => c.name === b.name)) return err(409, "conflict", "component name already exists");
          const c = { id: `cmp_${randomUUID().slice(0, 8)}`, name: b.name, kind, description: b.description ?? "", createdAt: now() };
          components.set(c.id, c);
          revisions.set(c.id, []);
          return send(201, c);
        }
      } else if (!components.has(id)) return err(404, "not_found", "component not found");
      else if (!sub) {
        const c = components.get(id)!;
        if (method === "GET") return send(200, c);
        if (method === "PATCH") { if (typeof b.description === "string") c.description = b.description; return send(200, c); }
        if (method === "DELETE") {
          if (revisions.get(id)!.length) return err(409, "conflict", "component has revisions");
          components.delete(id);
          return send(204);
        }
      } else if (sub === "revisions") {
        const list = revisions.get(id)!;
        if (!subId) {
          if (method === "GET") return send(200, { items: list });
          if (method === "POST") {
            if (!b.content || typeof b.content !== "object") return err(400, "bad_request", "content must be a JSON object or array");
            const r = { id: `rev_${randomUUID().slice(0, 8)}`, componentId: id, number: list.length + 1, digest: `sha256:${createHash("sha256").update(canon(b.content)).digest("hex")}`, content: b.content, createdAt: now() };
            list.push(r);
            return send(201, r);
          }
        } else if (method === "GET") {
          const n = Number(subId);
          if (!Number.isInteger(n) || n < 1) return err(400, "bad_request", "revision number must be a positive integer");
          const r = list[n - 1];
          return r ? send(200, r) : err(404, "not_found", "revision not found");
        }
      }
    }

    if (res === "runtimes") {
      if (!id && method === "GET") return send(200, { items: [...runtimes.values()].filter((r) => !url.searchParams.get("environment") || r.environment === url.searchParams.get("environment")) });
      if (!id && method === "POST") {
        if (!b.id || !b.name || !b.environment) return err(400, "bad_request", "id, name, environment required");
        const r = { status: "online", labels: {}, lastSeenAt: now(), ...b };
        runtimes.set(b.id, r);
        return send(200, r);
      }
      if (id && !runtimes.has(id)) return err(404, "not_found", "runtime not found");
      if (id && !sub && method === "GET") return send(200, runtimes.get(id));
      if (id && sub === "desired-state" && method === "GET") return send(200, desired.get(id) ?? { runtimeId: id, generation: 0, items: {} });
    }

    if (res === "deployments") {
      if (!id && method === "GET") {
        const rt = url.searchParams.get("runtimeId");
        const cp = url.searchParams.get("componentId");
        return send(200, { items: [...deployments.values()].filter((d) => (!rt || d.runtimeId === rt) && (!cp || d.componentId === cp)) });
      }
      if (!id && method === "POST") {
        if (!b.componentId || !b.runtimeId) return err(400, "bad_request", "componentId and runtimeId must be non-empty strings");
        if (!components.has(b.componentId)) return err(404, "not_found", "component not found");
        const rt = runtimes.get(b.runtimeId);
        if (!rt) return err(404, "not_found", "runtime not found");
        let rev = b.revisionId ? revisions.get(b.componentId)!.find((r) => r.id === b.revisionId) : undefined;
        if (!b.revisionId) {
          if (typeof b.revision !== "number") return err(400, "bad_request", "revisionId or revision (number) required");
          rev = revisions.get(b.componentId)![b.revision - 1];
        }
        if (!rev) return err(404, "not_found", "revision not found");
        const ds = desired.get(b.runtimeId) ?? { runtimeId: b.runtimeId, environment: rt.environment, generation: 0, items: {}, updatedAt: now() };
        ds.generation++;
        ds.items[b.componentId] = rev.id;
        ds.updatedAt = now();
        desired.set(b.runtimeId, ds);
        const d = { id: `dep_${randomUUID().slice(0, 8)}`, componentId: b.componentId, revisionId: rev.id, runtimeId: b.runtimeId, environment: rt.environment, generation: ds.generation, status: "pending", message: "", createdAt: now(), updatedAt: now() };
        deployments.set(d.id, d);
        return send(202, { deployment: d, desiredState: ds });
      }
      if (id && !deployments.has(id)) return err(404, "not_found", "deployment not found");
      if (id && !sub && method === "GET") return send(200, deployments.get(id));
      if (id && sub === "status" && method === "PUT") {
        if (!["pending", "running", "failed", "stopped"].includes(b.status)) return err(400, "bad_request", "invalid status");
        const d = deployments.get(id)!;
        d.status = b.status;
        d.message = b.message ?? "";
        d.updatedAt = now();
        return send(200, d);
      }
    }
    return err(res ? 405 : 404, res ? "method_not_allowed" : "not_found", "no such route");
  });

  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const port = (server.address() as AddressInfo).port;
  return {
    url: `http://127.0.0.1:${port}`,
    requests,
    failNext(count, status, headers = {}) { failures = { count, status, headers }; },
    hangNext() { hang = true; },
    addRuntime(r) { runtimes.set(r.id, { status: "online", labels: {}, lastSeenAt: new Date().toISOString(), ...r }); },
    close: () => new Promise<void>((r) => { server.closeAllConnections(); server.close(() => r()); }),
  };
}
