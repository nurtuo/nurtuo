// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { once } from "node:events";
import { request, type Server } from "node:http";
import { test } from "node:test";
import { createHandler, createHttpServer, digestOf } from "./api.js";
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

async function listen(server: Server) {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address && typeof address === "object");
  return `http://127.0.0.1:${address.port}`;
}

test("HTTP skips dispatch when the client disconnects mid-body", { timeout: 5000 }, async (t) => {
  const dispatched: string[] = [];
  const server = createHttpServer(new MemoryRepository(), { auth: {
    async handle(req) {
      dispatched.push(req.path);
      return { status: 200, body: { ok: true } };
    },
  } });
  t.after(() => { server.closeAllConnections(); server.close(); });
  const base = await listen(server);
  const received = Promise.withResolvers<void>();
  const closed = Promise.withResolvers<void>();
  server.once("request", (req) => {
    req.once("data", () => received.resolve());
    req.once("close", () => closed.resolve());
  });
  const client = request(base + "/partial", {
    method: "POST",
    headers: { "content-type": "application/json", "content-length": "10" },
  });
  client.on("error", () => {});
  client.write("{");
  await received.promise;
  client.destroy();
  await closed.promise;

  const next = await fetch(base + "/complete");
  assert.equal(next.status, 200);
  assert.deepEqual(await next.json(), { ok: true });
  assert.deepEqual(dispatched, ["/complete"]);
});

test("HTTP without auth returns 404 for workspace ID paths", async (t) => {
  const server = createHttpServer(new MemoryRepository(), { auth: "disabled" });
  t.after(() => server.close());
  const base = await listen(server);
  for (const [method, path, status] of [
    ["GET", "/v1/workspaces/ws", 404],
    ["PUT", "/v1/workspaces/ws/connections/sftp", 404],
    ["POST", "/v1/workspaces/ws/test-runs", 404],
    ["GET", "/v1/workspaces", 405],
    ["PUT", "/v1/components", 405],
  ] as const) {
    const response = await fetch(base + path, { method });
    assert.equal(response.status, status, path);
    assert.equal(asAny(await response.json()).error.code, status === 404 ? "not_found" : "method_not_allowed");
  }
});

test("HTTP aborts the handler signal when the client disconnects before the response", { timeout: 5000 }, async (t) => {
  let handlerStarted!: (signal: AbortSignal) => void;
  const started = new Promise<AbortSignal>((resolve) => { handlerStarted = resolve; });
  const server = createHttpServer(new MemoryRepository(), { auth: {
    async handle(req) {
      assert.ok(req.signal);
      if (req.path === "/complete") return { status: 200, body: { aborted: req.signal.aborted } };
      handlerStarted(req.signal);
      await once(req.signal, "abort");
      return { status: 204 };
    },
  } });
  t.after(() => { server.closeAllConnections(); server.close(); });
  const base = await listen(server);
  const client = request(base + "/disconnect");
  client.on("error", () => {});
  client.end();
  const signal = await started;
  assert.equal(signal.aborted, false);
  const aborted = once(signal, "abort");
  client.destroy();
  await aborted;
  assert.equal(signal.aborted, true);
  assert.deepEqual(await (await fetch(base + "/complete")).json(), { aborted: false });
});
