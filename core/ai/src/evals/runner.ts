// SPDX-License-Identifier: AGPL-3.0-only
// Runs a scenario with an agent and grades the *stored* flow independently of what the agent claims.
import type { Flow } from "@nurtuo/flow-schema";
import { findLiteralSecrets, type DryRunner } from "@nurtuo/mcp";
import { hasErrors, validateFlow } from "@nurtuo/flow-validate";
import type { Agent } from "./agents.js";
import { EvalSession } from "./session.js";
import type { Check, Expectations, Scenario, ScenarioResult } from "./scenario.js";

export interface RunOptions {
  runtime?: DryRunner;
}

export async function runScenario(scenario: Scenario, agent: Agent, opts: RunOptions = {}): Promise<ScenarioResult> {
  const started = Date.now();
  const session = await EvalSession.open({ ...(opts.runtime ? { runtime: opts.runtime } : {}) });
  const checks: Check[] = [];
  let flow: Flow | undefined;
  try {
    if (scenario.setup) await scenario.setup(session.store);
    const prompt = await session.prompt("build-integration", { goal: scenario.goal, flowId: scenario.flowId });
    try {
      await agent.run({ scenario, prompt }, session);
    } catch (e) {
      checks.push({ name: "agent completed", passed: false, detail: e instanceof Error ? e.message : String(e) });
    }
    flow = await session.store.getFlow(scenario.flowId);
    checks.push(...grade(flow, scenario.expect, session));
  } finally {
    await session.close();
  }
  return {
    scenario: scenario.id, agent: agent.name, passed: checks.every((c) => c.passed), checks,
    toolCalls: session.toolCalls, flow, durationMs: Date.now() - started,
  };
}

export function grade(flow: Flow | undefined, e: Expectations, session?: EvalSession): Check[] {
  const checks: Check[] = [];
  const add = (name: string, passed: boolean, detail?: string) => checks.push({ name, passed, ...(detail && !passed ? { detail } : {}) });
  if (!flow) {
    add("flow exists", false, "the agent never stored the flow");
    return checks;
  }
  add("flow exists", true);

  const diags = validateFlow(flow);
  const expectValid = e.valid ?? true;
  add(expectValid ? "flow validates" : "flow is invalid as expected", hasErrors(diags) !== expectValid,
    diags.filter((d) => d.severity === "error").map((d) => `${d.code}@${d.path}`).join(", "));

  if (e.minRevision !== undefined) add(`revision >= ${e.minRevision}`, flow.revision >= e.minRevision, `revision is ${flow.revision}`);

  for (const [kind, n] of Object.entries(e.minStepKinds ?? {})) {
    const have = flow.steps.filter((s) => s.kind === kind).length;
    add(`>= ${n} ${kind} step(s)`, have >= (n ?? 0), `found ${have}`);
  }
  for (const c of e.connectors ?? []) {
    const hit = flow.steps.some((s) => s.kind === "connector" && s.config["connector"] === c.connector
      && String(s.config["operation"]).toUpperCase() === c.operation.toUpperCase());
    add(`uses ${c.connector}.${c.operation}`, hit);
  }
  if (e.start) {
    const start = flow.steps.find((s) => s.kind === "start");
    const bad = Object.entries(e.start).filter(([k, v]) => start?.config[k] !== v).map(([k]) => k);
    add("start step matches", bad.length === 0, `differs in ${bad.join(", ")}`);
  }
  for (const k of e.edges ?? []) add(`edge ${k}`, k in flow.edges);
  if (e.pathKinds) add(`path through ${e.pathKinds.join(" > ")}`, hasPath(flow, e.pathKinds), "no start-to-end path visits those kinds in order");

  add("no literal secrets in the flow", findLiteralSecrets(flow.steps.map((s) => s.config)).length === 0);
  if (session) {
    add("no literal secrets in tool results", findLiteralSecrets(session.transcript.map((t) => t.result)).length === 0);
    if (e.maxToolCalls !== undefined) add(`<= ${e.maxToolCalls} tool calls`, session.toolCalls <= e.maxToolCalls, `used ${session.toolCalls}`);
  }
  return checks;
}

/** True if some path from the start step visits `kinds` as an ordered subsequence. */
function hasPath(flow: Flow, kinds: string[]): boolean {
  const byId = new Map(flow.steps.map((s) => [s.id, s]));
  const next = (id: string) => Object.entries(flow.edges).filter(([k]) => k.startsWith(`${id}:`)).map(([, to]) => to);
  const walk = (id: string, i: number, seen: Set<string>): boolean => {
    if (seen.has(id)) return false;
    const step = byId.get(id);
    if (!step) return false;
    const j = step.kind === kinds[i] ? i + 1 : i;
    if (j >= kinds.length) return true;
    const s2 = new Set(seen).add(id);
    return next(id).some((n) => walk(n, j, s2));
  };
  const start = flow.steps.find((s) => s.kind === "start");
  return start ? walk(start.id, 0, new Set()) : false;
}

export function formatReport(results: ScenarioResult[]): string {
  return results.map((r) => {
    const head = `${r.passed ? "PASS" : "FAIL"} ${r.scenario} [${r.agent}] ${r.toolCalls} tool calls, ${r.durationMs} ms`;
    const bad = r.checks.filter((c) => !c.passed).map((c) => `  - ${c.name}${c.detail ? `: ${c.detail}` : ""}`);
    return [head, ...bad].join("\n");
  }).join("\n");
}
