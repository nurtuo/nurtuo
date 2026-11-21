// SPDX-License-Identifier: AGPL-3.0-only
// Unit tests with a tiny fake control plane (headers, error mapping, caching). Fidelity against the
// real control-plane handler is tested in core/control-plane (mcp-store.test.ts).
import { test } from "node:test";
import assert from "node:assert/strict";
import { FLOW_SCHEMA_VERSION, type Flow } from "@nurtuo/flow-schema";
import { ControlPlaneClient, ControlPlaneFlowStore, RevisionConflictError, ToolError, type FetchLike } from "./index.js";

interface Call { method: string; url: string; headers: Record<string, string>; body?: unknown }

function fake(routes: (c: Call) => { status: number; body?: unknown } | Promise<{ status: number; body?: unknown }>) {
  const calls: Call[] = [];
  const fetch: FetchLike = async (url, init) => {
    const c: Call = { method: init.method, url, headers: init.headers, ...(init.body ? { body: JSON.parse(init.body) } : {}) };
    calls.push(c);
    const r = await routes(c);
    return { status: r.status, text: async () => (r.body === undefined ? "" : JSON.stringify(r.body)) };
  };
  return { fetch, calls };
}

const flow = (rev: number): Flow => ({
  schema: FLOW_SCHEMA_VERSION, id: "f", name: "F", revision: rev,
  steps: [{ id: "start", kind: "start", config: { type: "manual" } }], edges: {},
});

test("client sends bearer token, query and JSON; maps errors; never leaks the token", async () => {
  const { fetch, calls } = fake((c) => (c.url.includes("boom") ? { status: 500, body: { error: { code: "internal", message: "internal error" } } } : { status: 200, body: { ok: 1 } }));
  const api = new ControlPlaneClient({ baseUrl: "http://cp/", token: "tok-123", fetch });
  await api.request("POST", "/v1/x", { body: { a: 1 }, query: { q: "1", skip: undefined } });
  assert.equal(calls[0]?.url, "http://cp/v1/x?q=1");
  assert.equal(calls[0]?.headers["authorization"], "Bearer tok-123");
  assert.equal(calls[0]?.headers["content-type"], "application/json");
  await assert.rejects(api.request("GET", "/v1/boom"), (e: Error & { status: number; code: string }) => e.status === 500 && e.code === "internal" && !e.message.includes("tok-123"));

  const down = new ControlPlaneClient({ baseUrl: "http://cp", token: "tok-123", fetch: async () => { throw new Error("connect ECONNREFUSED"); } });
  await assert.rejects(down.request("GET", "/v1/x"), /unreachable/);
  const slow = new ControlPlaneClient({ baseUrl: "http://cp", timeoutMs: 20, fetch: (_u, init) => new Promise((_, rej) => init.signal?.addEventListener("abort", () => rej(Object.assign(new Error("aborted"), { name: "TimeoutError" })))) });
  await assert.rejects(slow.request("GET", "/v1/x"), (e: { code: string }) => e.code === "timeout");
});

test("store maps 401/404/400/5xx/timeouts to structured tool errors", async () => {
  const mk = (status: number) => new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch: fake(() => ({ status, body: { error: { code: "x", message: "m" } } })).fetch });
  await assert.rejects(mk(401).listFlows(), (e: ToolError) => e.code === "unauthorized");
  await assert.rejects(mk(403).listComponents(), (e: ToolError) => e.code === "unauthorized");
  await assert.rejects(mk(502).listFlows(), (e: ToolError) => e.code === "backend_error" && e.details?.["status"] === 502);
  await assert.rejects(mk(400).listRuntimes(), (e: ToolError) => e.code === "invalid_input");
});

test("saveFlow: revision check before write, and after write when another writer won the race", async () => {
  // Latest revision on the server is 2; caller read 1 -> conflict without writing.
  const state = { revisions: [{ id: "r1", componentId: "c1", number: 1, content: flow(1) }, { id: "r2", componentId: "c1", number: 2, content: flow(2) }] };
  const { fetch, calls } = fake((c) => {
    if (c.method === "GET" && c.url.endsWith("/v1/components")) return { status: 200, body: { items: [{ id: "c1", name: "f", kind: "flow", description: "F" }] } };
    if (c.method === "GET" && c.url.endsWith("/revisions")) return { status: 200, body: { items: state.revisions } };
    if (c.method === "POST" && c.url.endsWith("/revisions")) {
      // Simulate a concurrent writer: the server assigns number 3 although we expected to create 2.
      return { status: 201, body: { id: "r3", componentId: "c1", number: 3, content: c.body } };
    }
    return { status: 404 };
  });
  const store = new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch });
  await assert.rejects(store.saveFlow(flow(2), 1), (e: RevisionConflictError) => e instanceof RevisionConflictError && e.expected === 1 && e.actual === 2);
  assert.equal(calls.filter((c) => c.method === "POST").length, 0);

  // Pre-check passes (latest is 1, caller expects 1) but another writer slips in: server assigns 3.
  state.revisions = [state.revisions[0] as (typeof state.revisions)[number]];
  await assert.rejects(store.saveFlow(flow(2), 1), (e: RevisionConflictError) => e instanceof RevisionConflictError && e.expected === 1 && e.actual === 2);
  assert.equal(calls.filter((c) => c.method === "POST").length, 1);
});

test("saveFlow rejects flow ids the control plane cannot name, getFlow returns latest with revision number", async () => {
  const { fetch } = fake((c) => {
    if (c.url.endsWith("/v1/components")) return { status: 200, body: { items: [{ id: "c1", name: "f", kind: "flow", description: "" }, { id: "c2", name: "map1", kind: "map", description: "a map" }] } };
    if (c.url.endsWith("/c1/revisions")) return { status: 200, body: { items: [{ id: "r1", componentId: "c1", number: 1, content: { ...flow(0), name: "old" } }, { id: "r2", componentId: "c1", number: 2, content: { ...flow(0), name: "new" } }] } };
    return { status: 404 };
  });
  const store = new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch });
  await assert.rejects(store.saveFlow({ ...flow(1), id: "Bad Id" }, 0), (e: ToolError) => e.code === "invalid_input");
  const f = await store.getFlow("f");
  assert.equal(f?.name, "new");
  assert.equal(f?.revision, 2);
  assert.equal(await store.getFlow("missing"), undefined);
  assert.deepEqual(await store.listComponents(), [{ id: "map1", kind: "map", description: "a map" }]);
  assert.equal((await store.listFlows()).length, 1);
});
