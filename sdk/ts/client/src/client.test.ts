// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { test } from "node:test";
import { ApiError, NetworkError, NurtuoClient } from "./index.js";
import { startFakeControlPlane } from "./testing.js";

test("components, revisions, runtimes, deploy, status round trip", async () => {
  const cp = await startFakeControlPlane({ token: "s3cret" });
  try {
    const c = new NurtuoClient({ baseUrl: cp.url + "/", token: "s3cret", retryDelayMs: 1 });
    const comp = await c.createComponent({ name: "orders", description: "d" });
    assert.equal(comp.kind, "flow");
    assert.equal((await c.listComponents()).length, 1);
    assert.equal((await c.updateComponent(comp.id!, { description: "x" })).description, "x");
    const r1 = await c.createRevision(comp.id!, { b: 1, a: 2 });
    const r2 = await c.createRevision(comp.id!, { a: 2, b: 1 });
    assert.deepEqual([r1.number, r2.number], [1, 2]);
    assert.equal(r1.digest, r2.digest);
    assert.equal((await c.listRevisions(comp.id!)).length, 2);
    assert.equal((await c.getRevision(comp.id!, 2)).id, r2.id);

    cp.addRuntime({ id: "rt1", name: "rt", environment: "dev" });
    assert.equal((await c.listRuntimes({ environment: "dev" })).length, 1);
    assert.equal((await c.listRuntimes({ environment: "prod" })).length, 0);
    assert.equal((await c.getRuntime("rt1")).name, "rt");
    assert.equal((await c.registerRuntime({ id: "rt2", name: "n", environment: "prod" })).id, "rt2");

    const res = await c.deploy({ componentId: comp.id!, runtimeId: "rt1", revision: 2 });
    assert.equal(res.deployment.revisionId, r2.id);
    assert.equal(res.desiredState.generation, 1);
    assert.equal((await c.getDesiredState("rt1")).items?.[comp.id!], r2.id);
    assert.equal((await c.getDeployment(res.deployment.id!)).status, "pending");
    assert.equal((await c.reportDeploymentStatus(res.deployment.id!, "running", "ok")).status, "running");
    assert.equal((await c.listDeployments({ runtimeId: "rt1" })).length, 1);
    assert.equal((await c.listDeployments({ runtimeId: "zzz" })).length, 0);

    await assert.rejects(c.deleteComponent(comp.id!), (e: unknown) => e instanceof ApiError && e.status === 409 && e.code === "conflict");
    assert.ok(cp.requests.every((r) => r.authorization === "Bearer s3cret"));
  } finally {
    await cp.close();
  }
});

test("delete returns void on 204; path segments are encoded", async () => {
  const cp = await startFakeControlPlane();
  try {
    const c = new NurtuoClient({ baseUrl: cp.url });
    const comp = await c.createComponent({ name: "tmp" });
    assert.equal(await c.deleteComponent(comp.id!), undefined);
    await assert.rejects(c.getComponent("a/b"), (e: unknown) => e instanceof ApiError && e.status === 404);
    assert.equal(cp.requests.at(-1)!.path, "/v1/components/a%2Fb");
  } finally {
    await cp.close();
  }
});

test("errors carry status, code and message; missing token is 401", async () => {
  const cp = await startFakeControlPlane({ token: "t" });
  try {
    await assert.rejects(new NurtuoClient({ baseUrl: cp.url }).listComponents(), (e: unknown) => e instanceof ApiError && e.status === 401 && e.code === "unauthorized");
    const c = new NurtuoClient({ baseUrl: cp.url, token: "t" });
    await assert.rejects(c.createComponent({ name: "Bad Name" }), (e: unknown) => e instanceof ApiError && e.status === 400 && /name must match/.test(e.message));
  } finally {
    await cp.close();
  }
});

test("idempotent requests retry on 503 and honor Retry-After; POST does not retry", async () => {
  const cp = await startFakeControlPlane();
  try {
    const c = new NurtuoClient({ baseUrl: cp.url, retries: 2, retryDelayMs: 1 });
    cp.failNext(2, 503, { "retry-after": "0" });
    assert.deepEqual(await c.listComponents(), []);
    assert.equal(cp.requests.length, 3);

    cp.failNext(3, 503);
    await assert.rejects(c.listComponents(), (e: unknown) => e instanceof ApiError && e.status === 503);

    cp.requests.length = 0;
    cp.failNext(1, 503);
    await assert.rejects(c.createComponent({ name: "x" }), (e: unknown) => e instanceof ApiError && e.status === 503);
    assert.equal(cp.requests.length, 1);

    const unsafe = new NurtuoClient({ baseUrl: cp.url, retryUnsafe: true, retryDelayMs: 1 });
    cp.failNext(1, 502);
    assert.equal((await unsafe.createComponent({ name: "y" })).name, "y");
  } finally {
    await cp.close();
  }
});

test("4xx is not retried", async () => {
  const cp = await startFakeControlPlane();
  try {
    const c = new NurtuoClient({ baseUrl: cp.url, retryDelayMs: 1 });
    await assert.rejects(c.getComponent("nope"));
    assert.equal(cp.requests.length, 1);
  } finally {
    await cp.close();
  }
});

test("timeouts and connection errors surface as NetworkError", async () => {
  const cp = await startFakeControlPlane();
  try {
    const c = new NurtuoClient({ baseUrl: cp.url, timeoutMs: 50, retries: 0 });
    cp.hangNext();
    await assert.rejects(c.listComponents(), (e: unknown) => e instanceof NetworkError && e.timedOut);
  } finally {
    await cp.close();
  }
  const dead = new NurtuoClient({ baseUrl: "http://127.0.0.1:1", retries: 1, retryDelayMs: 1 });
  await assert.rejects(dead.listComponents(), (e: unknown) => e instanceof NetworkError && !e.timedOut);
});

test("baseUrl is required", () => {
  assert.throws(() => new NurtuoClient({ baseUrl: "" }), TypeError);
});
