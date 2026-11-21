// SPDX-License-Identifier: AGPL-3.0-only
import { test } from "node:test";
import assert from "node:assert/strict";
import { FLOW_SCHEMA_VERSION, type Flow } from "@nurtuo/flow-schema";
import { InMemoryDeployer, InMemoryFlowStore, MCP_TOOLS_DEPLOY, MCP_TOOLS_V1, MCP_TOOLS_V2, type DryRunner } from "./index.js";
import { connectClient } from "./testkit.js";

const ops = [
  { op: "addStep", step: { id: "start", kind: "start", config: { type: "listener", connector: "http", path: "/e", method: "GET" } } },
  { op: "addStep", after: { step: "start", port: "default" }, step: { id: "lookup", kind: "connector", config: { connector: "postgres", operation: "QUERY", objectType: "employee" } } },
  { op: "addStep", after: { step: "lookup", port: "default" }, step: { id: "done", kind: "returnDocuments", config: {} } },
];

test("list_step_kinds and get_step_schema expose kinds with config schemas from flow-schema", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  const kinds = (await c.call("list_step_kinds")).body.kinds as { kind: string; ports: string; terminal: boolean }[];
  assert.equal(kinds.length, 15);
  assert.equal(kinds.find((k) => k.kind === "decision")?.ports, "true, false");
  assert.equal(kinds.find((k) => k.kind === "stop")?.terminal, true);

  const s = (await c.call("get_step_schema", { kind: "decision" })).body;
  assert.deepEqual(s.configSchema.required, ["cel"]);
  assert.equal(s.exampleStep.kind, "decision");
  const branch = (await c.call("get_step_schema", { kind: "branch" })).body;
  assert.equal(branch.configSchema.properties.paths.minimum, 2);
  assert.equal((await c.call("get_step_schema", { kind: "nope" })).isError, true);
  await c.close();
});

test("example steps from get_step_schema are valid when placed in a flow", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  const kinds = (await c.call("list_step_kinds")).body.kinds as { kind: string }[];
  for (const { kind } of kinds) {
    if (kind === "start") continue;
    const ex = (await c.call("get_step_schema", { kind })).body.exampleStep;
    const id = `f-${kind.toLowerCase()}`;
    const r = await c.call("apply_ops", { flowId: id, create: { name: kind }, ops: [
      { op: "addStep", step: { id: "start", kind: "start", config: { type: "manual" } } },
      { op: "addStep", after: { step: "start", port: "default" }, step: ex },
    ] });
    // Unconnected ports are expected; there must be no schema errors.
    const schemaErrors = (r.body.diagnostics as { code: string }[]).filter((d) => d.code.startsWith("schema."));
    assert.deepEqual(schemaErrors, [], `${kind}: ${JSON.stringify(schemaErrors)}`);
  }
  await c.close();
});

test("list_connectors covers the four native connectors", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  const { connectors } = (await c.call("list_connectors")).body;
  assert.deepEqual(connectors.map((x: { id: string }) => x.id), ["http", "nats", "postgres", "sftp"]);
  const sftp = connectors.find((x: { id: string }) => x.id === "sftp");
  assert.ok(sftp.operations.some((o: { operation: string }) => o.operation === "CREATE"));
  assert.ok(sftp.connectionFields.includes("hostKeyPolicy"));
  assert.equal(sftp.connectionSchema, undefined);
  const http = (await c.call("list_connectors", { id: "http", includeConnectionSchema: true })).body.connectors[0];
  assert.equal(http.connectionSchema.required[0], "baseUrl");
  assert.deepEqual(http.operations.map((o: { operation: string }) => o.operation), ["GET", "POST", "PUT", "PATCH", "DELETE"]);
  assert.equal((await c.call("list_connectors", { id: "ftp" })).body.code, "not_found");
  await c.close();
});

test("suggest_next_steps proposes ops that close open ports until the flow is done", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  await c.call("apply_ops", { flowId: "f", create: { name: "F" }, ops: [ops[0]] });
  let r = (await c.call("suggest_next_steps", { flowId: "f" })).body;
  assert.equal(r.done, false);
  assert.deepEqual(r.openPorts, [{ step: "start", kind: "start", port: "default" }]);
  assert.ok(r.fixes.some((f: { code: string }) => f.code === "step.port_unconnected"));

  // Following the first candidate each time converges.
  for (let i = 0; i < 10 && !r.done; i++) {
    const op = r.suggestions[0].candidates[0].op;
    assert.equal((await c.call("apply_ops", { flowId: "f", ops: [op] })).body.applied, true);
    r = (await c.call("suggest_next_steps", { flowId: "f" })).body;
  }
  assert.equal(r.done, true);
  assert.equal(r.valid, true);

  // decision false port suggests stop.
  await c.call("apply_ops", { flowId: "d", create: { name: "D" }, ops: [
    ops[0], { op: "addStep", after: { step: "start", port: "default" }, step: { id: "check", kind: "decision", config: { cel: "true" } } },
  ] });
  const d = (await c.call("suggest_next_steps", { flowId: "d" })).body;
  const falsePort = d.suggestions.find((s: { at: { port: string } }) => s.at.port === "false");
  assert.equal(falsePort.candidates[0].kind, "stop");

  // empty store: no start step -> firstStep
  await c.call("apply_ops", { flowId: "e", create: { name: "E" }, ops: [{ op: "addStep", step: { id: "x", kind: "map", config: { jsonata: "$" } } }] });
  const e = (await c.call("suggest_next_steps", { flowId: "e" })).body;
  assert.equal(e.firstStep.op.step.kind, "start");
  assert.equal((await c.call("suggest_next_steps", { flowId: "zzz" })).body.code, "not_found");
  await c.close();
});

test("explain_diagnostic explains known codes and gives concrete ops with a flow", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  await c.call("apply_ops", { flowId: "f", create: { name: "F" }, ops: ops.slice(0, 2) });
  const v = (await c.call("validate", { flowId: "f" })).body;
  const d = v.diagnostics.find((x: { code: string }) => x.code === "step.port_unconnected");
  const e = (await c.call("explain_diagnostic", { code: d.code, path: d.path, stepId: d.stepId, flowId: "f" })).body;
  assert.equal(e.known, true);
  assert.match(e.explanation, /port/i);
  assert.equal(e.exampleOps[0].op, "addStep");
  assert.deepEqual(e.exampleOps[0].after, { step: "lookup", port: "default" });
  const unknown = (await c.call("explain_diagnostic", { code: "made.up" })).body;
  assert.equal(unknown.known, false);
  await c.close();
});

test("diff_flows compares revisions and inline flows", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  await c.call("apply_ops", { flowId: "f", create: { name: "F" }, ops });
  await c.call("apply_ops", { flowId: "f", ops: [
    { op: "updateConfig", id: "lookup", patch: { objectType: "person" } },
    { op: "addStep", after: { step: "lookup", port: "default" }, step: { id: "shape", kind: "map", config: { jsonata: "$" } } },
  ] });
  const d = (await c.call("diff_flows", { left: { flowId: "f", revision: 1 }, right: { flowId: "f" } })).body;
  assert.equal(d.identical, false);
  assert.deepEqual(d.stepsAdded, [{ id: "shape", kind: "map" }]);
  assert.deepEqual(d.stepsChanged[0].changes, [{ path: "/config/objectType", before: "employee", after: "person" }]);
  assert.ok(d.edgesAdded.includes("shape:default -> done"));
  assert.ok(d.edgesRetargeted.includes("lookup:default: done -> shape"));
  assert.match(d.summary, /\+ step shape \(map\)/);

  const same = (await c.call("diff_flows", { left: { flowId: "f" }, right: { flowId: "f" } })).body;
  assert.equal(same.identical, true);
  const inline = (await c.call("diff_flows", { left: { flowId: "f" }, right: { flow: { steps: [], edges: {} } } })).body;
  assert.equal(inline.stepsRemoved.length, 4);
  assert.equal((await c.call("diff_flows", { left: { flowId: "f", revision: 9 }, right: { flowId: "f" } })).body.code, "not_found");
  assert.equal((await c.call("diff_flows", { left: {}, right: { flowId: "f" } })).body.code, "invalid_input");
  await c.close();
});

test("guardrails: ops limit, input size, step limit, secrets in input, structured codes", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore(), limits: { maxOps: 3, maxInputBytes: 4000, maxSteps: 3 } });
  const tooMany = await c.call("apply_ops", { flowId: "f", create: { name: "F" }, ops: [...ops, ops[2]!] });
  assert.equal(tooMany.isError, true);
  assert.equal(tooMany.body.code, "limit_exceeded");
  assert.equal(tooMany.body.limit, "maxOps");

  const big = await c.call("apply_ops", { flowId: "f", create: { name: "F" }, ops: [{ op: "addStep", step: { id: "m", kind: "map", config: { jsonata: "x".repeat(5000) } } }] });
  assert.equal(big.body.code, "limit_exceeded");
  assert.equal(big.body.limit, "maxInputBytes");

  const steps = await c.call("apply_ops", { flowId: "s", create: { name: "S" }, ops: [
    { op: "addStep", step: { id: "a", kind: "map", config: { jsonata: "$" } } },
    { op: "addStep", step: { id: "b", kind: "map", config: { jsonata: "$" } } },
    { op: "addStep", step: { id: "c", kind: "map", config: { jsonata: "$" } } },
  ] });
  assert.equal(steps.body.applied, true);
  const over = await c.call("apply_ops", { flowId: "s", ops: [{ op: "addStep", step: { id: "d", kind: "map", config: { jsonata: "$" } } }] });
  assert.equal(over.body.code, "limit_exceeded");
  assert.equal(over.body.limit, "maxSteps");

  const secret = await c.call("apply_ops", { flowId: "x", create: { name: "X" }, ops: [
    { op: "addStep", step: { id: "m", kind: "map", config: { jsonata: "{ 'h': 'Bearer abcdefghijklmnop' }" } } },
  ] });
  assert.equal(secret.body.code, "secret_in_input");
  assert.deepEqual(secret.body.paths, ["/ops/0/step/config/jsonata"]);
  assert.equal((await c.call("get_flow", { flowId: "x" })).body.code, "not_found");
  await c.close();
});

test("outputs are redacted even when the store holds secrets", async () => {
  const store = new InMemoryFlowStore();
  const flow: Flow = {
    schema: FLOW_SCHEMA_VERSION, id: "leaky", name: "Leaky", revision: 0,
    steps: [
      { id: "start", kind: "start", config: { type: "manual" } },
      { id: "m", kind: "map", config: { jsonata: "{ 'u': 'https://bob:hunter2@example.com/x' }" } },
      { id: "end", kind: "returnDocuments", config: {} },
    ],
    edges: { "start:default": "m", "m:default": "end" },
  };
  await store.saveFlow({ ...flow, revision: 1 }, 0);
  // Bypass validation shape: a stored (schema-invalid) config key with a secret.
  await store.saveFlow({ ...flow, revision: 2, steps: flow.steps.map((s) => (s.id === "m" ? { ...s, config: { ...s.config, password: "p4ss" } } : s)) }, 1);
  const c = await connectClient({ store });
  const g = await c.call("get_flow", { flowId: "leaky" });
  const text = JSON.stringify(g.body);
  assert.ok(!text.includes("hunter2"), text);
  assert.ok(!text.includes("p4ss"), text);
  assert.ok(text.includes("[REDACTED]"));
  const diff = await c.call("diff_flows", { left: { flowId: "leaky", revision: 1 }, right: { flowId: "leaky" } });
  assert.ok(!JSON.stringify(diff.body).includes("p4ss"));
  await c.close();
});

test("per-call timeout yields a timeout error", async () => {
  const hang: DryRunner = { dryRun: () => new Promise(() => {}) };
  const c = await connectClient({ store: new InMemoryFlowStore(), runtime: hang, limits: { callTimeoutMs: 40 } });
  await c.call("apply_ops", { flowId: "f", create: { name: "F" }, ops });
  const r = await c.call("dry_run", { flowId: "f" });
  assert.equal(r.isError, true);
  assert.equal(r.body.code, "timeout");
  await c.close();
});

test("output size limit is enforced", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore(), limits: { maxOutputBytes: 200 } });
  const r = await c.call("list_step_kinds");
  assert.equal(r.body.code, "limit_exceeded");
  assert.equal(r.body.limit, "maxOutputBytes");
  await c.close();
});

test("deploy tools: absent by default, deploy needs --allow-deploy, valid flow and confirmation", async () => {
  const store = new InMemoryFlowStore();
  const deployer = new InMemoryDeployer(store);

  const ro = await connectClient({ store, deployer });
  const names = (await ro.client.listTools()).tools.map((t) => t.name);
  assert.ok(names.includes("deployment_status") && names.includes("list_runtimes"));
  assert.ok(!names.includes("deploy"));
  await ro.close();

  const c = await connectClient({ store, deployer, allowDeploy: true });
  const all = (await c.client.listTools()).tools.map((t) => t.name).sort();
  assert.deepEqual(all, [...MCP_TOOLS_V1, ...MCP_TOOLS_V2, ...MCP_TOOLS_DEPLOY].sort());
  const dep = (await c.client.listTools()).tools.find((t) => t.name === "deploy");
  assert.equal(dep?.annotations?.destructiveHint, true);

  await c.call("apply_ops", { flowId: "f", create: { name: "F" }, ops: ops.slice(0, 2) });
  const invalid = await c.call("deploy", { flowId: "f", runtimeId: "local" });
  assert.equal(invalid.body.code, "invalid_flow");
  await c.call("apply_ops", { flowId: "f", ops: [ops[2]] });

  assert.equal((await c.call("deploy", { flowId: "f", runtimeId: "nope" })).body.code, "not_found");
  assert.equal((await c.call("deploy", { flowId: "zzz", runtimeId: "local" })).body.code, "not_found");

  const plan = await c.call("deploy", { flowId: "f", runtimeId: "local" });
  assert.equal(plan.isError, false);
  assert.equal(plan.body.requiresConfirmation, true);
  assert.equal(plan.body.deployed, false);
  assert.equal((await c.call("deployment_status", { flowId: "f" })).body.total, 0);

  const done = (await c.call("deploy", { flowId: "f", runtimeId: "local", confirm: true })).body;
  assert.equal(done.deployed, true);
  assert.equal(done.deployment.status, "pending");
  assert.equal(done.deployment.revision, 2);

  deployer.setStatus(done.deployment.id, "running");
  assert.equal((await c.call("deployment_status", { deploymentId: done.deployment.id })).body.deployment.status, "running");
  assert.equal((await c.call("deployment_status", { flowId: "f" })).body.deployments.length, 1);
  assert.equal((await c.call("deployment_status", { deploymentId: "nope" })).body.code, "not_found");
  const rts = (await c.call("list_runtimes")).body.runtimes;
  assert.equal(rts[0].id, "local");
  await c.close();
});

test("resources expose flows and catalogs", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  await c.call("apply_ops", { flowId: "get-employee", create: { name: "GetEmployee" }, ops });
  const { resources } = await c.client.listResources();
  assert.ok(resources.some((r) => r.uri === "nurtuo://flows/get-employee" && r.title === "GetEmployee"));
  assert.ok(resources.some((r) => r.uri === "nurtuo://catalog/connectors"));
  const { resourceTemplates } = await c.client.listResourceTemplates();
  assert.deepEqual(resourceTemplates.map((t) => t.uriTemplate).sort(), ["nurtuo://flows/{flowId}", "nurtuo://flows/{flowId}/summary"]);

  const doc = await c.client.readResource({ uri: "nurtuo://flows/get-employee" });
  const flow = JSON.parse((doc.contents[0] as { text: string }).text);
  assert.equal(flow.id, "get-employee");
  assert.equal(flow.steps.length, 3);
  assert.equal(doc.contents[0]?.mimeType, "application/json");

  const sum = await c.client.readResource({ uri: "nurtuo://flows/get-employee/summary" });
  assert.match((sum.contents[0] as { text: string }).text, /lookup \(connector\) => default:done/);
  const kinds = await c.client.readResource({ uri: "nurtuo://catalog/step-kinds" });
  assert.equal(JSON.parse((kinds.contents[0] as { text: string }).text).kinds.length, 15);
  await assert.rejects(c.client.readResource({ uri: "nurtuo://flows/missing" }), /not found/);
  await c.close();
});

test("prompts: build-integration, fix-flow, explain-flow", async () => {
  const c = await connectClient({ store: new InMemoryFlowStore() });
  const { prompts } = await c.client.listPrompts();
  assert.deepEqual(prompts.map((p) => p.name).sort(), ["build-integration", "explain-flow", "fix-flow"]);
  const bi = prompts.find((p) => p.name === "build-integration");
  assert.deepEqual(bi?.arguments?.map((a) => [a.name, a.required === true]), [["goal", true], ["source", false], ["target", false], ["flowId", false]]);

  const r = await c.client.getPrompt({ name: "build-integration", arguments: { goal: "Copy orders to SFTP nightly", source: "HTTP API", target: "SFTP", flowId: "orders-sftp" } });
  const text = (r.messages[0]?.content as { text: string }).text;
  assert.equal(r.messages[0]?.role, "user");
  for (const needle of ["Copy orders to SFTP nightly", "HTTP API", "SFTP", "orders-sftp", "list_connectors", "apply_ops", "suggest_next_steps", "dry_run", "never put passwords"]) {
    assert.ok(text.toLowerCase().includes(needle.toLowerCase()), `prompt mentions ${needle}`);
  }
  const fix = await c.client.getPrompt({ name: "fix-flow", arguments: { flowId: "abc" } });
  assert.match((fix.messages[0]?.content as { text: string }).text, /"abc"/);
  await assert.rejects(c.client.getPrompt({ name: "build-integration", arguments: {} }));
  await c.close();
});
