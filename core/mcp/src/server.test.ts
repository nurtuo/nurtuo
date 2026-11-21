// SPDX-License-Identifier: AGPL-3.0-only
import { test } from "node:test";
import assert from "node:assert/strict";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { createServer, InMemoryFlowStore, MCP_TOOLS_V1, MCP_TOOLS_V2, type DryRunner } from "./index.js";

const fakeRuntime: DryRunner = {
  async dryRun(flow, input) {
    return { ok: true, trace: flow.steps.map((s) => ({ stepId: s.id })), output: { echo: input } };
  },
};

async function connect(runtime?: DryRunner) {
  const store = new InMemoryFlowStore([{ id: "salesforce", kind: "connector", description: "Salesforce CRM connector" }]);
  const server = createServer({ store, ...(runtime ? { runtime } : {}) });
  const [ct, st] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: "test", version: "0.0.0" });
  await Promise.all([server.connect(st), client.connect(ct)]);
  const call = async (name: string, args: Record<string, unknown>) => {
    const r = await client.callTool({ name, arguments: args });
    const text = (r.content as { text: string }[])[0]?.text ?? "";
    return { isError: r.isError === true, body: JSON.parse(text) as any }; // eslint-disable-line @typescript-eslint/no-explicit-any
  };
  return { client, call };
}

const getEmployeeOps = [
  { op: "addStep", step: { id: "start", kind: "start", config: { type: "listener", connector: "http", path: "/employee", method: "GET" } } },
  { op: "addStep", after: { step: "start", port: "default" }, step: { id: "lookup", kind: "connector", label: "Get employee", config: { connector: "salesforce", operation: "query", objectType: "Employee" } } },
  { op: "addStep", after: { step: "lookup", port: "default" }, step: { id: "done", kind: "returnDocuments", config: {} } },
];

test("lists the v1 and v2 tools, but no deploy tools without a deployer", async () => {
  const { client } = await connect();
  const { tools } = await client.listTools();
  assert.deepEqual(tools.map((t) => t.name).sort(), [...MCP_TOOLS_V1, ...MCP_TOOLS_V2].sort());
});

test("agent builds a GetEmployee flow, validates and dry-runs it", async () => {
  const { call } = await connect(fakeRuntime);

  const applied = await call("apply_ops", { flowId: "get-employee", create: { name: "GetEmployee" }, ops: getEmployeeOps });
  assert.equal(applied.isError, false);
  assert.equal(applied.body.applied, true);
  assert.equal(applied.body.valid, true, JSON.stringify(applied.body.diagnostics));
  assert.equal(applied.body.revision, 1);

  const view = (await call("get_flow", { flowId: "get-employee" })).body;
  assert.deepEqual(view.edges, ["start:default -> lookup", "lookup:default -> done"]);
  assert.deepEqual(view.steps[0].ports, [{ port: "default", to: "lookup" }]);

  const v = (await call("validate", { flowId: "get-employee" })).body;
  assert.equal(v.valid, true);

  const found = (await call("search", { query: "employee" })).body;
  assert.deepEqual(found.flows.map((f: { id: string }) => f.id), ["get-employee"]);
  assert.equal((await call("search", { query: "salesforce" })).body.components.length, 1);

  const run = await call("dry_run", { flowId: "get-employee", input: { id: 7 } });
  assert.equal(run.isError, false);
  assert.equal(run.body.ran, true);
  assert.deepEqual(run.body.result.output, { echo: { id: 7 } });
  assert.deepEqual(run.body.result.trace.map((t: { stepId: string }) => t.stepId), ["start", "lookup", "done"]);
});

test("apply_ops is atomic and reports diagnostics for incomplete flows", async () => {
  const { call } = await connect(fakeRuntime);
  await call("apply_ops", { flowId: "f", create: { name: "F" }, ops: getEmployeeOps.slice(0, 2) });
  const bad = await call("apply_ops", {
    flowId: "f",
    ops: [{ op: "setLabel", id: "start", label: "x" }, { op: "removeStep", id: "nope" }],
  });
  assert.equal(bad.isError, true);
  assert.equal(bad.body.failedOpIndex, 1);
  assert.equal(bad.body.code, "step_not_found");
  const view = (await call("get_flow", { flowId: "f" })).body;
  assert.equal(view.steps[0].label, undefined); // first op rolled back
  assert.equal(view.revision, 1);

  const v = (await call("validate", { flowId: "f" })).body;
  assert.equal(v.valid, false);
  assert.ok(v.diagnostics.some((d: { code: string }) => d.code === "step.port_unconnected"));

  const run = await call("dry_run", { flowId: "f" });
  assert.equal(run.isError, true);
  assert.equal(run.body.ran, false);
});

test("dry_run without a runtime and unknown flows report errors", async () => {
  const { call } = await connect();
  await call("apply_ops", { flowId: "g", create: { name: "G" }, ops: getEmployeeOps });
  const run = await call("dry_run", { flowId: "g" });
  assert.equal(run.isError, true);
  assert.match(run.body.reason, /no runtime/);
  assert.equal((await call("get_flow", { flowId: "zzz" })).isError, true);
  assert.equal((await call("apply_ops", { flowId: "zzz", ops: getEmployeeOps })).isError, true);
});
