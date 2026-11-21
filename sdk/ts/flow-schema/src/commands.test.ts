// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { applyOps, CommandBus, type Flow } from "./index.js";

const base = (): Flow => ({
  schema: "nurtuo.flow/v1",
  id: "t",
  name: "T",
  revision: 1,
  steps: [
    { id: "start", kind: "start", config: { type: "manual" } },
    { id: "end", kind: "stop", config: {} },
  ],
  edges: { "start:default": "end" },
});

test("addStep appends a step", () => {
  const r = applyOps(base(), [{ op: "addStep", step: { id: "m", kind: "message", config: { template: "x" } } }]);
  assert.ok(r.ok);
  assert.deepEqual(r.flow.steps.map((s) => s.id), ["start", "end", "m"]);
});

test("addStep with after splices into an occupied port", () => {
  const r = applyOps(base(), [
    { op: "addStep", step: { id: "m", kind: "message", config: { template: "x" } }, after: { step: "start", port: "default" } },
  ]);
  assert.ok(r.ok);
  assert.deepEqual(r.flow.edges, { "start:default": "m", "m:default": "end" });
});

test("addStep with after on a free port connects", () => {
  const f = { ...base(), edges: {} };
  const r = applyOps(f, [{ op: "addStep", step: { id: "m", kind: "message", config: { template: "x" } }, after: { step: "start", port: "default" } }]);
  assert.ok(r.ok);
  assert.deepEqual(r.flow.edges, { "start:default": "m" });
});

test("addStep rejects duplicate id and bad id", () => {
  const dup = applyOps(base(), [{ op: "addStep", step: { id: "end", kind: "stop", config: {} } }]);
  assert.ok(!dup.ok && dup.code === "step_exists" && dup.index === 0);
  const bad = applyOps(base(), [{ op: "addStep", step: { id: "Bad Id", kind: "stop", config: {} } }]);
  assert.ok(!bad.ok && bad.code === "invalid_id");
});

test("removeStep drops the step and every edge touching it", () => {
  const r = applyOps(base(), [{ op: "removeStep", id: "end" }]);
  assert.ok(r.ok);
  assert.deepEqual(r.flow.steps.map((s) => s.id), ["start"]);
  assert.deepEqual(r.flow.edges, {});
});

test("updateConfig is a merge patch: null deletes a key, objects merge", () => {
  const f = base();
  f.steps[1] = { id: "end", kind: "stop", config: { status: 404, message: "m" } };
  const r = applyOps(f, [{ op: "updateConfig", id: "end", patch: { status: 410, message: null } }]);
  assert.ok(r.ok);
  assert.deepEqual(r.flow.steps[1]?.config, { status: 410 });
});

test("connect, disconnect and occupied ports", () => {
  const f = { ...base(), edges: {} };
  const c = applyOps(f, [{ op: "connect", from: "start", port: "default", to: "end" }]);
  assert.ok(c.ok);
  assert.deepEqual(c.flow.edges, { "start:default": "end" });
  const again = applyOps(c.flow, [{ op: "connect", from: "start", port: "default", to: "end" }]);
  assert.ok(!again.ok && again.code === "port_occupied");
  const d = applyOps(c.flow, [{ op: "disconnect", from: "start", port: "default" }]);
  assert.ok(d.ok);
  assert.deepEqual(d.flow.edges, {});
  const missing = applyOps(d.flow, [{ op: "disconnect", from: "start", port: "default" }]);
  assert.ok(!missing.ok && missing.code === "edge_not_found");
});

test("connect to unknown step fails", () => {
  const r = applyOps({ ...base(), edges: {} }, [{ op: "connect", from: "start", port: "default", to: "ghost" }]);
  assert.ok(!r.ok && r.code === "step_not_found");
});

test("renameStep rewrites edges; setLabel sets and clears", () => {
  const r = applyOps(base(), [{ op: "renameStep", id: "end", newId: "done" }, { op: "setLabel", id: "done", label: "Done" }]);
  assert.ok(r.ok);
  assert.deepEqual(r.flow.edges, { "start:default": "done" });
  assert.equal(r.flow.steps[1]?.label, "Done");
  const cleared = applyOps(r.flow, [{ op: "setLabel", id: "done", label: null }]);
  assert.ok(cleared.ok);
  assert.equal("label" in (cleared.flow.steps[1] ?? {}), false);
  const clash = applyOps(base(), [{ op: "renameStep", id: "end", newId: "start" }]);
  assert.ok(!clash.ok && clash.code === "step_exists");
});

test("a batch is atomic and the input is never mutated", () => {
  const f = base();
  const snapshot = JSON.stringify(f);
  const r = applyOps(f, [
    { op: "addStep", step: { id: "m", kind: "message", config: { template: "x" } } },
    { op: "removeStep", id: "ghost" },
  ]);
  assert.ok(!r.ok && r.index === 1 && r.code === "step_not_found");
  assert.equal(JSON.stringify(f), snapshot);
  const ok = applyOps(f, [{ op: "setLabel", id: "end", label: "x" }]);
  assert.ok(ok.ok);
  assert.equal(JSON.stringify(f), snapshot);
});

test("CommandBus supports undo/redo, notifies, and ignores failed dispatches", () => {
  const bus = new CommandBus(base());
  const seen: number[] = [];
  const off = bus.subscribe((f) => seen.push(f.steps.length));
  assert.equal(bus.canUndo, false);
  assert.ok(bus.dispatch([{ op: "addStep", step: { id: "m", kind: "message", config: { template: "x" } } }]).ok);
  assert.equal(bus.flow.steps.length, 3);
  assert.ok(!bus.dispatch([{ op: "removeStep", id: "nope" }]).ok);
  assert.equal(bus.flow.steps.length, 3);
  assert.equal(bus.undo(), true);
  assert.equal(bus.flow.steps.length, 2);
  assert.equal(bus.canRedo, true);
  assert.equal(bus.redo(), true);
  assert.equal(bus.flow.steps.length, 3);
  assert.ok(bus.dispatch([{ op: "setLabel", id: "m", label: "L" }]).ok);
  assert.equal(bus.canRedo, false);
  off();
  bus.undo();
  assert.deepEqual(seen, [3, 2, 3, 3]);
});
