// SPDX-License-Identifier: AGPL-3.0-only
import type { Flow } from "@nurtuo/flow-schema";
import { describe, expect, it, vi } from "vitest";
import { STEP_CATALOG, configSchemaOf, emptyFlow, newStep } from "./catalog";
import { checkConnection } from "./connection";
import { flowMeta, flowToGraph, graphToFlow, graphToPositions, parseLoaded, serializeFlow } from "./flowAdapter";
import { FlowStore, configPatch } from "./flowStore";
import { fieldsOf, fromText } from "./formSchema";

const sample = (): Flow => ({
  schema: "nurtuo.flow/v1",
  id: "demo",
  name: "Demo",
  description: "d",
  revision: 3,
  steps: [
    { id: "start-1", kind: "start", config: { type: "manual" } },
    { id: "check", kind: "decision", label: "Big?", config: { cel: "doc.n > 1" } },
    { id: "ok", kind: "returnDocuments", config: {} },
    { id: "no", kind: "stop", config: { status: 400 } },
  ],
  edges: { "start-1:default": "check", "check:true": "ok", "check:false": "no" },
});

describe("flow <-> graph adapter", () => {
  it("round-trips flow -> graph -> flow", () => {
    const f = sample();
    const g = flowToGraph(f);
    expect(g.nodes.map((n) => n.id)).toEqual(["start-1", "check", "ok", "no"]);
    expect(g.nodes[1]?.data.ports).toEqual(["true", "false"]);
    expect(g.edges).toContainEqual({ id: "check:false", source: "check", sourceHandle: "false", target: "no" });
    expect(graphToFlow(flowMeta(f), g)).toEqual(f);
    expect(JSON.parse(serializeFlow(graphToFlow(flowMeta(f), g)))).toEqual(f);
  });

  it("keeps stored positions and auto-lays out the rest", () => {
    const g = flowToGraph(sample(), { check: { x: 7, y: 9 } });
    expect(g.nodes.find((n) => n.id === "check")?.position).toEqual({ x: 7, y: 9 });
    const start = g.nodes[0]?.position;
    const ok = g.nodes[2]?.position;
    expect(ok?.y).toBeGreaterThan(start?.y ?? 0);
    expect(graphToPositions(g)["check"]).toEqual({ x: 7, y: 9 });
  });

  it("round-trips an empty flow and omits absent optionals", () => {
    const f = emptyFlow();
    expect(graphToFlow(flowMeta(f), flowToGraph(f))).toEqual(f);
  });

  it("parses flow and layout JSON, rejects others", () => {
    expect(parseLoaded(JSON.stringify(sample())).flow?.id).toBe("demo");
    expect(parseLoaded('{"flowId":"x","positions":{}}').layout?.flowId).toBe("x");
    expect(() => parseLoaded("[]")).toThrow();
    expect(() => parseLoaded('{"a":1}')).toThrow();
  });
});

describe("FlowStore (CommandBus backed)", () => {
  it("adds, connects, undoes and redoes through the bus", () => {
    const s = new FlowStore(emptyFlow());
    const fn = vi.fn();
    s.subscribe(fn);
    const start = newStep(s.getState().flow, "start");
    const stop = newStep(s.getState().flow, "stop");
    expect(s.dispatch([{ op: "addStep", step: start }, { op: "addStep", step: stop }]).ok).toBe(true);
    expect(s.dispatch([{ op: "connect", from: start.id, port: "default", to: stop.id }]).ok).toBe(true);
    expect(s.getState().flow.edges).toEqual({ [`${start.id}:default`]: stop.id });
    expect(s.getState().diagnostics.filter((d) => d.severity === "error")).toEqual([]);
    expect(s.undo()).toBe(true);
    expect(s.getState().flow.edges).toEqual({});
    expect(s.getState().canRedo).toBe(true);
    s.redo();
    expect(Object.keys(s.getState().flow.edges)).toHaveLength(1);
    expect(s.getState().positions[start.id]).toBeDefined();
    expect(fn).toHaveBeenCalled();
  });

  it("restores edges when a removed step is undone and reports op errors", () => {
    const s = new FlowStore(sample());
    expect(s.dispatch([{ op: "removeStep", id: "check" }]).ok).toBe(true);
    expect(s.getState().flow.edges).toEqual({});
    s.undo();
    expect(Object.keys(s.getState().flow.edges)).toHaveLength(3);
    const bad = s.dispatch([{ op: "connect", from: "check", port: "true", to: "no" }]);
    expect(bad.ok).toBe(false);
  });

  it("surfaces validator diagnostics with step ids", () => {
    const s = new FlowStore(emptyFlow());
    s.dispatch([{ op: "addStep", step: newStep(s.getState().flow, "decision") }]);
    const d = s.getState().diagnostics;
    // empty CEL fails the schema stage; the offending step is attributed for highlighting
    expect(d.some((x) => x.code.startsWith("schema.") && x.stepId === "decision-1")).toBe(true);
    s.dispatch([{ op: "updateConfig", id: "decision-1", patch: { cel: "true" } }]);
    expect(s.getState().diagnostics.some((x) => x.code === "flow.start_count")).toBe(true);
    expect(s.getState().diagnostics.some((x) => x.stepId === "decision-1")).toBe(true);
  });

  it("load replaces the document and clears history", () => {
    const s = new FlowStore(emptyFlow());
    s.dispatch([{ op: "addStep", step: newStep(emptyFlow(), "start") }]);
    s.load(sample(), { check: { x: 1, y: 2 } });
    expect(s.getState().canUndo).toBe(false);
    expect(s.getState().flow.id).toBe("demo");
    expect(s.getState().positions["check"]).toEqual({ x: 1, y: 2 });
  });

  it("config edit then undo restores the stored config (regression: stale editor text)", () => {
    const s = new FlowStore(sample());
    const before = s.getState().flow.steps[1]?.config ?? {};
    const patch = configPatch(before, { cel: "x" });
    s.dispatch([{ op: "updateConfig", id: "check", patch }]);
    expect(s.getState().flow.steps[1]?.config).toEqual({ cel: "x" });
    s.undo();
    expect(s.getState().flow.steps[1]?.config).toEqual(before);
  });
});

describe("configPatch", () => {
  it("emits null for removed keys and recurses objects", () => {
    expect(configPatch({ a: 1, b: { c: 1, d: 2 } }, { b: { c: 2, d: 2 } })).toEqual({ a: null, b: { c: 2 } });
    expect(configPatch({ a: [1] }, { a: [1, 2] })).toEqual({ a: [1, 2] });
    expect(configPatch({ a: 1 }, { a: 1 })).toEqual({});
  });
});

describe("checkConnection", () => {
  it("validates ports, occupancy, start targets and cycles", () => {
    const f = sample();
    expect(checkConnection(f, { source: "ok", port: "default", target: "no" }).ok).toBe(false); // terminal
    expect(checkConnection(f, { source: "check", port: "true", target: "no" }).ok).toBe(false); // occupied
    expect(checkConnection(f, { source: "no", port: "default", target: "start-1" }).ok).toBe(false);
    const g: Flow = { ...f, steps: [...f.steps, { id: "m", kind: "map", config: { jsonata: "x" } }], edges: { ...f.edges, "ok:default": "m" } };
    expect(checkConnection(g, { source: "m", port: "default", target: "check" })).toEqual({ ok: false, reason: "Would create a cycle" });
    expect(checkConnection(g, { source: "m", port: "default", target: "no" }).ok).toBe(true);
  });
});

describe("catalog and form schema", () => {
  it("builds the palette from all step kinds with schemas", () => {
    expect(STEP_CATALOG).toHaveLength(15);
    expect(fieldsOf(configSchemaOf("start")).map((f) => f.name)).toContain("type");
  });

  it("derives field types and parses text", () => {
    const start = fieldsOf(configSchemaOf("start"));
    expect(start.find((f) => f.name === "type")).toMatchObject({ type: "enum", required: true });
    const paths = fieldsOf(configSchemaOf("branch"))[0]!;
    expect(paths).toMatchObject({ type: "integer", min: 2, max: 25 });
    expect(fromText(paths, "3")).toEqual({ ok: true, value: 3 });
    expect(fromText(paths, "2.5").ok).toBe(false);
    const routes = fieldsOf(configSchemaOf("route"))[0]!;
    expect(routes.type).toBe("json");
    expect(fromText(routes, '[{"id":"a","cel":"true"}]').ok).toBe(true);
    expect(fromText(routes, "[").ok).toBe(false);
    const cron = start.find((f) => f.name === "cron")!;
    expect(fromText(cron, "")).toEqual({ ok: true, value: undefined });
  });

  it("generated default steps pass schema-level id rules", () => {
    const f = emptyFlow();
    expect(newStep(f, "setProperties").id).toBe("setproperties-1");
  });
});

describe("exported flow carries no secret", () => {
  it("serializeFlow of a connector step has only the connection id", () => {
    const f: Flow = {
      ...emptyFlow(),
      steps: [{ id: "connector-1", kind: "connector", config: { connector: "sftp", operation: "GET", connection: "rebex-demo" } }],
    };
    const out = serializeFlow(f);
    expect(JSON.parse(out).steps[0].config).toEqual({ connector: "sftp", operation: "GET", connection: "rebex-demo" });
    expect(out).not.toMatch(/password|secret:/i);
  });
});
