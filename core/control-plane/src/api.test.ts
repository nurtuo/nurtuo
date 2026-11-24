// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import type { AddressInfo } from "node:net";
import { MAX_BODY_BYTES, createHandler, createHttpServer, digestOf } from "./api.js";
import { MemoryRepository } from "./memory-repository.js";

function setup() {
  const h = createHandler(new MemoryRepository());
  const call = (method: string, path: string, body?: unknown, query?: Record<string, string>) => h({ method, path, body, query });
  return { call };
}
const asAny = (x: unknown) => x as any;

test("component CRUD and validation", async () => {
  const { call } = setup();
  assert.equal((await call("POST", "/v1/components", { name: "Bad Name" })).status, 400);
  const c = await call("POST", "/v1/components", { name: "orders", description: "d" });
  assert.equal(c.status, 201);
  assert.equal((await call("POST", "/v1/components", { name: "orders" })).status, 409);
  const id = asAny(c.body).id;
  assert.equal(asAny((await call("GET", "/v1/components")).body).items.length, 1);
  assert.equal(asAny((await call("PATCH", `/v1/components/${id}`, { description: "x" })).body).description, "x");
  assert.equal((await call("GET", "/v1/components/nope")).status, 404);
  assert.equal((await call("DELETE", `/v1/components/${id}`)).status, 204);
});

test("revisions are numbered, immutable and digest-addressed", async () => {
  const { call } = setup();
  const id = asAny((await call("POST", "/v1/components", { name: "f" })).body).id;
  const r1 = asAny((await call("POST", `/v1/components/${id}/revisions`, { content: { b: 1, a: 2 } })).body);
  const r2 = asAny((await call("POST", `/v1/components/${id}/revisions`, { content: { a: 2, b: 1 } })).body);
  assert.deepEqual([r1.number, r2.number], [1, 2]);
  assert.equal(r1.digest, r2.digest);
  assert.equal(r1.digest, digestOf({ a: 2, b: 1 }));
  assert.equal((await call("POST", `/v1/components/${id}/revisions`, { content: "str" })).status, 400);
  assert.equal(asAny((await call("GET", `/v1/components/${id}/revisions/2`)).body).id, r2.id);
  assert.equal((await call("GET", `/v1/components/${id}/revisions/9`)).status, 404);
  assert.equal((await call("DELETE", `/v1/components/${id}`)).status, 409);
});

test("deploy updates desired state, creates deployment and outbox, status reported", async () => {
  const repo = new MemoryRepository();
  const h = createHandler(repo);
  const call = (method: string, path: string, body?: unknown, query?: Record<string, string>) => h({ method, path, body, query });
  await call("POST", "/v1/runtimes", { id: "rt1", name: "edge", environment: "prod" });
  await call("POST", "/v1/runtimes", { id: "rt2", name: "dev", environment: "dev" });
  assert.equal(asAny((await call("GET", "/v1/runtimes", undefined, { environment: "prod" })).body).items.length, 1);
  const id = asAny((await call("POST", "/v1/components", { name: "f" })).body).id;
  await call("POST", `/v1/components/${id}/revisions`, { content: { v: 1 } });

  const d = await call("POST", "/v1/deployments", { componentId: id, revision: 1, runtimeId: "rt1" });
  assert.equal(d.status, 202);
  const dep = asAny(d.body).deployment;
  assert.equal(dep.status, "pending");
  assert.equal(dep.generation, 1);
  const ds = asAny((await call("GET", "/v1/runtimes/rt1/desired-state")).body);
  assert.equal(ds.generation, 1);
  assert.equal(Object.keys(ds.items).length, 1);
  assert.equal((await repo.listOutbox({ unpublishedOnly: true })).length, 1);

  await call("POST", `/v1/components/${id}/revisions`, { content: { v: 2 } });
  const d2 = asAny((await call("POST", "/v1/deployments", { componentId: id, revision: 2, runtimeId: "rt1" })).body);
  assert.equal(d2.desiredState.generation, 2);

  assert.equal((await call("PUT", `/v1/deployments/${dep.id}/status`, { status: "bogus" })).status, 400);
  assert.equal(asAny((await call("PUT", `/v1/deployments/${dep.id}/status`, { status: "running" })).body).status, "running");
  assert.equal(asAny((await call("GET", "/v1/deployments", undefined, { runtimeId: "rt1" })).body).items.length, 2);
  assert.equal((await call("POST", "/v1/deployments", { componentId: id, revision: 1, runtimeId: "zz" })).status, 404);
  assert.equal(asAny((await call("GET", "/v1/runtimes/rt2/desired-state")).body).generation, 0);
});

test("unknown routes", async () => {
  const { call } = setup();
  assert.equal((await call("GET", "/nope")).status, 404);
  assert.equal((await call("PUT", "/v1/components")).status, 405);
});

test("revision accepts an externally supplied artifact digest", async () => {
  const { call } = setup();
  const id = asAny((await call("POST", "/v1/components", { name: "f" })).body).id;
  const digest = `sha256:${"ab".repeat(32)}`;
  const r = asAny((await call("POST", `/v1/components/${id}/revisions`, { content: { v: 1 }, digest })).body);
  assert.equal(r.digest, digest);
  assert.equal((await call("POST", `/v1/components/${id}/revisions`, { content: { v: 1 }, digest: "md5:x" })).status, 400);
});

test("undeploy removes the component from desired state and enqueues a change", async () => {
  const repo = new MemoryRepository();
  const h = createHandler(repo);
  const call = (method: string, path: string, body?: unknown) => h({ method, path, body });
  await call("POST", "/v1/runtimes", { id: "rt1", name: "edge", environment: "prod" });
  const id = asAny((await call("POST", "/v1/components", { name: "f" })).body).id;
  await call("POST", `/v1/components/${id}/revisions`, { content: { v: 1 } });
  await call("POST", "/v1/deployments", { componentId: id, revision: 1, runtimeId: "rt1" });
  assert.equal((await call("DELETE", "/v1/runtimes/nope/deployments/x")).status, 404);
  const r = await call("DELETE", `/v1/runtimes/rt1/deployments/${id}`);
  assert.equal(r.status, 202);
  assert.equal(asAny(r.body).generation, 2);
  assert.deepEqual(asAny(r.body).items, {});
  assert.equal((await call("DELETE", `/v1/runtimes/rt1/deployments/${id}`)).status, 404);
  assert.equal((await repo.listOutbox({ unpublishedOnly: true })).length, 2);
});

test("no-auth mode answers 404 (not 405) for workspace routes, including the canvas Run routes", async () => {
  const { call } = setup();
  for (const [m, p] of [
    ["GET", "/v1/workspaces/w1/connections"], ["PUT", "/v1/workspaces/w1/connections/c1"],
    ["POST", "/v1/workspaces/w1/test-runs"], ["GET", "/v1/workspaces/w1/members"],
  ] as const) {
    const r = await call(m, p, {});
    assert.equal(r.status, 404, `${m} ${p}`);
    assert.equal(asAny(r.body).error.code, "not_found");
  }
  // an unknown method on a known legacy resource is still 405
  assert.equal((await call("PUT", "/v1/components")).status, 405);
});

async function serve(opts: Parameters<typeof createHttpServer>[1]) {
  const srv = createHttpServer(new MemoryRepository(), opts);
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  return { srv, base: `http://127.0.0.1:${(srv.address() as AddressInfo).port}` };
}

test("body over 1 MiB is 413 before parsing; a body under the limit still works", async () => {
  const { srv, base } = await serve({ auth: "disabled" });
  try {
    // not JSON on purpose: a parse error (400) would mean the limit was applied after parsing
    const big = await fetch(`${base}/v1/workspaces/ws/test-runs`, { method: "POST", headers: { "content-type": "application/json" }, body: "x".repeat(MAX_BODY_BYTES + 1) });
    assert.equal(big.status, 413);
    assert.equal(asAny(await big.json()).error.code, "payload_too_large");
    const ok = await fetch(`${base}/v1/components`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ name: "small" }) });
    assert.equal(ok.status, 201);
  } finally { srv.close(); srv.closeAllConnections(); }
});

test("request close aborts ApiRequest.signal", async () => {
  const { setup: authSetup } = await import("./auth/test-util.js");
  const w = authSetup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  void ws;
  let seen: AbortSignal | undefined;
  let aborted!: () => void;
  const abortedP = new Promise<void>((r) => { aborted = r; });
  let entered!: () => void;
  const enteredP = new Promise<void>((r) => { entered = r; });
  const { createAuthHook } = await import("./auth/middleware.js");
  const hook = createAuthHook({
    service: w.service,
    extraEndpoints: { "connection.list": async (c) => { seen = c.signal; entered(); c.signal?.addEventListener("abort", aborted); await new Promise((r) => setTimeout(r, 3000)); return { status: 200, body: { items: [] } }; } },
  });
  const { srv, base } = await serve({ auth: hook });
  try {
    const login = await fetch(`${base}/v1/auth/login`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ email: "o@x.io", password: (await import("./auth/test-util.js")).PASSWORD }) });
    const cookie = /__Host-nurtuo_session=[^;]+/.exec(String(login.headers.get("set-cookie")))![0];
    const ac = new AbortController();
    const p = fetch(`${base}/v1/workspaces/${ws.id}/connections`, { headers: { cookie }, signal: ac.signal }).catch(() => undefined);
    await enteredP;
    assert.equal(seen?.aborted, false);
    ac.abort();
    await p;
    await Promise.race([abortedP, new Promise((_, rej) => setTimeout(() => rej(new Error("signal not aborted")), 2000))]);
    assert.equal(seen?.aborted, true);
  } finally { srv.close(); srv.closeAllConnections(); }
});
