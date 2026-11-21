// SPDX-License-Identifier: AGPL-3.0-only
import { test } from "node:test";
import assert from "node:assert/strict";
import { validateFlow, hasErrors } from "@nurtuo/flow-validate";
import {
  EvalSession, formatReport, getEmployee, getEmployeeOps, grade, httpMapSftp, httpMapSftpOps, repairFlow, runScenario,
  SCENARIOS, ScriptedAgent, SuggestionAgent, ToolLoopAgent,
  type Agent, type ChatMessage, type ChatModel, type Scenario,
} from "./index.js";

test("scripted reference solutions build valid flows for every scenario", async () => {
  const results = [];
  for (const { scenario, reference } of SCENARIOS) {
    results.push(await runScenario(scenario, new ScriptedAgent(reference, { create: scenario.setup === undefined })));
  }
  const report = formatReport(results);
  for (const r of results) {
    assert.equal(r.passed, true, report);
    assert.ok(r.flow && !hasErrors(validateFlow(r.flow)), `${r.scenario} validates independently of the agent`);
  }
  assert.match(report, /^PASS get-employee \[scripted\]/);
});

test("GetEmployee scenario: structure of the built flow", async () => {
  const r = await runScenario(getEmployee, new ScriptedAgent(getEmployeeOps));
  assert.equal(r.flow?.steps.length, 6);
  assert.equal(r.flow?.edges["found:false"], "not_found");
  assert.ok(r.checks.some((c) => c.name === "uses postgres.QUERY" && c.passed));
});

test("HTTP -> Map -> SFTP scenario uses the catalog operations", async () => {
  const r = await runScenario(httpMapSftp, new ScriptedAgent(httpMapSftpOps));
  assert.equal(r.passed, true, formatReport([r]));
  assert.ok(r.flow?.steps.some((s) => s.config["connector"] === "sftp" && s.config["operation"] === "CREATE"));
});

test("the grader fails agents that do nothing, stop early, or build the wrong thing", async () => {
  const idle: Agent = { name: "idle", async run() {} };
  const r1 = await runScenario(getEmployee, idle);
  assert.equal(r1.passed, false);
  assert.deepEqual(r1.checks.map((c) => c.name), ["flow exists"]);

  // Stops after the first batch: flow stored but invalid (open ports) and missing pieces.
  const r2 = await runScenario(getEmployee, new ScriptedAgent(getEmployeeOps.slice(0, 1)));
  assert.equal(r2.passed, false);
  const failed = r2.checks.filter((c) => !c.passed).map((c) => c.name);
  assert.ok(failed.includes("flow validates"));
  assert.ok(failed.includes(">= 1 map step(s)"));
  assert.match(formatReport([r2]), /^FAIL get-employee/);

  // Valid flow, wrong connector operation.
  const wrong = getEmployeeOps.map((b) => b.map((o) => JSON.parse(JSON.stringify(o).replace('"operation":"QUERY"', '"operation":"GET"'))));
  const r3 = await runScenario(getEmployee, new ScriptedAgent(wrong));
  assert.equal(r3.passed, false);
  assert.deepEqual(r3.checks.filter((c) => !c.passed).map((c) => c.name), ["uses postgres.QUERY"]);

  // Too many tool calls.
  const tight: Scenario = { ...getEmployee, expect: { ...getEmployee.expect, maxToolCalls: 2 } };
  const r4 = await runScenario(tight, new ScriptedAgent(getEmployeeOps));
  assert.deepEqual(r4.checks.filter((c) => !c.passed).map((c) => c.name), ["<= 2 tool calls"]);
});

test("repair scenario starts from the seeded broken flow", async () => {
  const s = await EvalSession.open();
  await repairFlow.setup!(s.store);
  const v = (await s.call("validate", { flowId: "broken-sync" })).body;
  assert.equal(v.valid, false);
  const codes = v.diagnostics.map((d: { code: string }) => d.code);
  assert.ok(codes.includes("edge.port_invalid"));
  assert.ok(codes.includes("step.port_unconnected"));
  await s.close();
  // And a do-nothing agent cannot pass it.
  assert.equal((await runScenario(repairFlow, { name: "idle", async run() {} })).passed, false);
});

test("suggestion-following agent converges to a valid flow without any scripted ops", async () => {
  const scenario: Scenario = {
    id: "suggest", title: "t", flowId: "auto", flowName: "Auto", goal: "any valid flow",
    expect: { minStepKinds: { start: 1 }, minRevision: 1 },
  };
  const r = await runScenario(scenario, new SuggestionAgent({ type: "manual" }));
  assert.equal(r.passed, true, formatReport([r]));
  assert.ok((r.flow?.steps.length ?? 0) >= 3);
});

test("secrets: literal credentials are refused and never reach the stored flow or tool results", async () => {
  const leaky: Agent = {
    name: "leaky",
    async run({ scenario }, s) {
      const r = await s.call("apply_ops", {
        flowId: scenario.flowId, create: { name: scenario.flowName },
        ops: [{ op: "addStep", step: { id: "start", kind: "start", config: { type: "manual" } } },
          { op: "addStep", after: { step: "start", port: "default" }, step: { id: "m", kind: "map", config: { jsonata: "'Bearer abcdefghijklmnop'" } } }],
      });
      assert.equal(r.body.code, "secret_in_input");
      await s.call("apply_ops", { flowId: scenario.flowId, create: { name: scenario.flowName }, ops: [{ op: "addStep", step: { id: "start", kind: "start", config: { type: "manual" } } }] });
    },
  };
  const scenario: Scenario = { ...getEmployee, id: "leak", expect: { valid: false } };
  const r = await runScenario(scenario, leaky);
  assert.ok(r.checks.find((c) => c.name === "no literal secrets in the flow")?.passed);
  assert.ok(r.checks.find((c) => c.name === "no literal secrets in tool results")?.passed);

  // The grader itself flags a flow that holds a secret.
  const bad = { ...r.flow!, steps: [{ id: "m", kind: "map" as const, config: { jsonata: "'Bearer abcdefghijklmnop'" } }] };
  assert.equal(grade(bad, {}).find((c) => c.name === "no literal secrets in the flow")?.passed, false);
});

/** A fake chat model that replays tool calls: stands in for an LLM to test the plug-in loop. */
function replayModel(batches: Record<string, unknown>[][]): ChatModel & { seen: { tools: string[]; firstPrompt: string; toolResults: number } } {
  let turn = 0;
  const seen = { tools: [] as string[], firstPrompt: "", toolResults: 0 };
  return {
    name: "replay", seen,
    async next(messages: ChatMessage[], tools) {
      if (turn === 0) {
        seen.tools = tools.map((t) => t.name);
        seen.firstPrompt = (messages[0] as { text: string }).text;
      }
      seen.toolResults = messages.filter((m) => m.role === "tool").length;
      const ops = batches[turn];
      if (!ops) return { text: "done" };
      const create = turn === 0 ? { create: { name: "GetEmployee" } } : {};
      const call = { id: `c${turn}`, name: "apply_ops", args: { flowId: "get-employee", ops, ...create } };
      turn++;
      return { toolCalls: [call] };
    },
  };
}

test("ToolLoopAgent: an LLM adapter sees the MCP tools and the guided prompt, and is graded like any agent", async () => {
  const model = replayModel(getEmployeeOps);
  const r = await runScenario(getEmployee, new ToolLoopAgent(model));
  assert.equal(r.passed, true, formatReport([r]));
  assert.equal(r.agent, "llm:replay");
  assert.ok(model.seen.tools.includes("apply_ops") && model.seen.tools.includes("suggest_next_steps"));
  assert.match(model.seen.firstPrompt, /Expose GET \/employees\/\{id\}/);
  assert.match(model.seen.firstPrompt, /list_connectors/);
  assert.equal(model.seen.toolResults, 2);

  // A model that gives up after one batch fails the scenario.
  const lazy = await runScenario(getEmployee, new ToolLoopAgent(replayModel(getEmployeeOps.slice(0, 1))));
  assert.equal(lazy.passed, false);

  // maxTurns bounds a model that never stops.
  const endless: ChatModel = { name: "endless", next: async () => ({ toolCalls: [{ id: "x", name: "validate", args: { flowId: "nope" } }] }) };
  const e = await runScenario(getEmployee, new ToolLoopAgent(endless, 3));
  assert.equal(e.toolCalls, 3);
});
