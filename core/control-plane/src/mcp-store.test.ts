// SPDX-License-Identifier: AGPL-3.0-only
// ControlPlaneFlowStore (from @nurtuo/mcp) against the real control-plane handler, in-process and
// over real HTTP, driven through the MCP server.
import assert from "node:assert/strict";
import type { AddressInfo } from "node:net";
import { test } from "node:test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { FLOW_SCHEMA_VERSION, type Flow } from "@nurtuo/flow-schema";
import { ControlPlaneFlowStore, createServer, RevisionConflictError, ToolError, type FetchLike } from "@nurtuo/mcp";
import { createHandler, createHttpServer } from "./api.js";
import { MemoryRepository } from "./memory-repository.js";

/** fetch implemented by calling the transport-agnostic handler directly. */
function inProcess(repo: MemoryRepository, opts: { token?: string } = {}): FetchLike {
  const handle = createHandler(repo);
  return async (url, init) => {
    const u = new URL(url);
    if (opts.token && init.headers["authorization"] !== `Bearer ${opts.token}`) {
      return { status: 401, text: async () => JSON.stringify({ error: { code: "unauthorized", message: "no" } }) };
    }
    const r = await handle({
      method: init.method, path: u.pathname, query: Object.fromEntries(u.searchParams),
      ...(init.body ? { body: JSON.parse(init.body) } : {}),
    });
    return { status: r.status, text: async () => (r.body === undefined ? "" : JSON.stringify(r.body)) };
  };
}

const flow = (id: string, rev: number): Flow => ({
  schema: FLOW_SCHEMA_VERSION, id, name: id.toUpperCase(), revision: rev,
  steps: [{ id: "start", kind: "start", config: { type: "manual" } }, { id: "end", kind: "returnDocuments", config: {} }],
  edges: { "start:default": "end" },
});

test("store round trip against the control-plane handler", async () => {
  const repo = new MemoryRepository();
  const store = new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch: inProcess(repo) });
  assert.equal(await store.getFlow("nope"), undefined);

  await store.saveFlow(flow("orders", 1), 0);
  await store.saveFlow({ ...flow("orders", 2), name: "Orders v2" }, 1);
  const latest = await store.getFlow("orders");
  assert.equal(latest?.revision, 2);
  assert.equal(latest?.name, "Orders v2");
  assert.equal((await store.getFlowRevision("orders", 1))?.name, "ORDERS");
  assert.equal(await store.getFlowRevision("orders", 9), undefined);

  // Stored as a flow component named after the flow id, with immutable numbered revisions.
  const comps = (await repo.listComponents()).filter((c) => c.name === "orders");
  assert.equal(comps.length, 1);
  assert.equal(comps[0]?.kind, "flow");
  assert.equal((await repo.listRevisions(comps[0]!.id)).length, 2);

  // Optimistic concurrency: stale expected revision, and creating an existing flow.
  await assert.rejects(store.saveFlow(flow("orders", 2), 1), (e: RevisionConflictError) => e instanceof RevisionConflictError && e.actual === 2 && e.expected === 1);
  await assert.rejects(store.saveFlow(flow("orders", 1), 0), RevisionConflictError);
  await assert.rejects(store.saveFlow(flow("fresh", 3), 2), RevisionConflictError, "unknown flow must be created at revision 0");

  // Non-flow components are listed as components; flows are not.
  await repo.createComponent({ name: "my-map", kind: "map", description: "A map" });
  assert.deepEqual(await store.listComponents(), [{ id: "my-map", kind: "map", description: "A map" }]);
  await store.saveFlow(flow("billing", 1), 0);
  assert.deepEqual((await store.listFlows()).map((f) => f.id).sort(), ["billing", "orders"]);

  await assert.rejects(store.saveFlow(flow("Bad Name", 1), 0), (e: ToolError) => e.code === "invalid_input");
});

test("two stores racing on the same flow: exactly one wins", async () => {
  const repo = new MemoryRepository();
  const a = new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch: inProcess(repo) });
  const b = new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch: inProcess(repo) });
  const results = await Promise.allSettled([a.saveFlow(flow("race", 1), 0), b.saveFlow(flow("race", 1), 0)]);
  assert.equal(results.filter((r) => r.status === "fulfilled").length, 1);
  const lost = results.find((r) => r.status === "rejected") as PromiseRejectedResult;
  assert.ok(lost.reason instanceof RevisionConflictError || (lost.reason as ToolError).code === "backend_error", String(lost.reason));
  assert.equal((await a.getFlow("race"))?.revision, results.some((r) => r.status === "rejected") ? 1 : 2);
});

test("MCP server on the control-plane store: build, conflict, diff, deploy, status", async () => {
  const repo = new MemoryRepository();
  await repo.upsertRuntime({ id: "rt1", name: "edge", environment: "prod", labels: {} });
  const store = new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch: inProcess(repo) });
  const server = createServer({ store, deployer: store, allowDeploy: true });
  const [ct, st] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: "t", version: "0" });
  await Promise.all([server.connect(st), client.connect(ct)]);
  const call = async (name: string, args: Record<string, unknown> = {}) => {
    const r = await client.callTool({ name, arguments: args });
    return { isError: r.isError === true, body: JSON.parse((r.content as { text: string }[])[0]!.text) };
  };

  const a = await call("apply_ops", { flowId: "get-employee", create: { name: "GetEmployee" }, ops: [
    { op: "addStep", step: { id: "start", kind: "start", config: { type: "manual" } } },
    { op: "addStep", after: { step: "start", port: "default" }, step: { id: "done", kind: "returnDocuments", config: {} } },
  ] });
  assert.equal(a.body.applied, true);
  assert.equal(a.body.valid, true);
  assert.equal(a.body.revision, 1);
  await call("apply_ops", { flowId: "get-employee", ops: [{ op: "setLabel", id: "start", label: "Go" }] });

  // Someone else writes revision 3 behind the MCP server's back; the next apply_ops (which read 2) is a clean conflict or a rebase on 3.
  const other = new ControlPlaneFlowStore({ baseUrl: "http://cp", fetch: inProcess(repo) });
  const cur = (await other.getFlow("get-employee"))!;
  await other.saveFlow({ ...cur, revision: 3, name: "Renamed" }, 2);
  const next = await call("apply_ops", { flowId: "get-employee", ops: [{ op: "setLabel", id: "done", label: "End" }] });
  assert.equal(next.body.applied, true);
  assert.equal(next.body.revision, 4, "re-reads the latest revision before applying");

  const diff = (await call("diff_flows", { left: { flowId: "get-employee", revision: 1 }, right: { flowId: "get-employee" } })).body;
  assert.ok(diff.metadata.some((m: { path: string; after?: string }) => m.path === "/name" && m.after === "Renamed"));
  assert.ok(diff.stepsChanged.some((s: { id: string }) => s.id === "start"));

  const search = (await call("search", { query: "get-employee" })).body;
  assert.equal(search.flows[0].revision, 4);

  const plan = (await call("deploy", { flowId: "get-employee", runtimeId: "rt1" })).body;
  assert.equal(plan.requiresConfirmation, true);
  assert.equal((await repo.listDeployments({})).length, 0);
  const dep = (await call("deploy", { flowId: "get-employee", runtimeId: "rt1", revision: 3, confirm: true })).body;
  assert.equal(dep.deployed, true);
  assert.equal(dep.deployment.flowId, "get-employee");
  assert.equal(dep.deployment.revision, 3);
  assert.equal(dep.deployment.status, "pending");

  await repo.updateDeploymentStatus(dep.deployment.id, "running", "ok");
  const status = (await call("deployment_status", { deploymentId: dep.deployment.id })).body.deployment;
  assert.equal(status.status, "running");
  assert.equal(status.flowId, "get-employee");
  assert.equal(status.revision, 3);
  assert.equal((await call("deployment_status", { flowId: "get-employee" })).body.deployments.length, 1);
  assert.equal((await call("list_runtimes")).body.runtimes[0].id, "rt1");
  assert.equal((await call("deploy", { flowId: "get-employee", runtimeId: "ghost" })).body.code, "not_found");

  const { resources } = await client.listResources();
  assert.ok(resources.some((r) => r.uri === "nurtuo://flows/get-employee"));
  await client.close();
});

test("over real HTTP with bearer auth", async () => {
  const repo = new MemoryRepository();
  const http = createHttpServer(repo, { auth: "disabled" });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${(http.address() as AddressInfo).port}`;
  try {
    const store = new ControlPlaneFlowStore({ baseUrl: base });
    await store.saveFlow(flow("web", 1), 0);
    assert.equal((await store.getFlow("web"))?.name, "WEB");
    assert.equal((await store.listFlows()).length, 1);
    const down = new ControlPlaneFlowStore({ baseUrl: "http://127.0.0.1:1", timeoutMs: 2000 });
    await assert.rejects(down.getFlow("x"), (e: ToolError) => e.code === "backend_error" || e.code === "timeout");
  } finally {
    http.close();
    http.closeAllConnections();
  }

  // Auth failures surface as `unauthorized`.
  const guarded = new ControlPlaneFlowStore({ baseUrl: "http://cp", token: "bad", fetch: inProcess(new MemoryRepository(), { token: "good" }) });
  await assert.rejects(guarded.listFlows(), (e: ToolError) => e.code === "unauthorized");
  const ok = new ControlPlaneFlowStore({ baseUrl: "http://cp", token: "good", fetch: inProcess(new MemoryRepository(), { token: "good" }) });
  assert.deepEqual(await ok.listFlows(), []);
});
