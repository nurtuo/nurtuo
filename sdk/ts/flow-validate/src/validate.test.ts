// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { validateFlow, hasErrors, portsOf } from "./index.js";

const example = JSON.parse(
  readFileSync(new URL("../../../../examples/flows/get-employee.flow.json", import.meta.url), "utf8"),
);

test("GetEmployee example is valid", () => {
  assert.deepEqual(validateFlow(example), []);
});

test("edge to unknown step is reported", () => {
  const broken = { ...example, edges: { ...example.edges, "to_response:default": "nowhere" } };
  assert.ok(validateFlow(broken).some((i) => i.code === "edge.target_unknown"));
});

test("hasErrors ignores warnings", () => {
  assert.equal(hasErrors([{ severity: "warning", code: "x", message: "m", path: "" }]), false);
  assert.equal(hasErrors([{ severity: "error", code: "x", message: "m", path: "" }]), true);
});

test("portsOf describes the ports of each step kind", () => {
  assert.deepEqual(portsOf({ id: "a", kind: "decision", config: {} }), ["true", "false"]);
  assert.deepEqual(portsOf({ id: "a", kind: "branch", config: { paths: 3 } }), ["1", "2", "3"]);
  assert.deepEqual(portsOf({ id: "a", kind: "route", config: { routes: [{ id: "x", cel: "1" }] } }), ["x", "default"]);
  assert.deepEqual(portsOf({ id: "a", kind: "tryCatch", config: {} }), ["try", "catch"]);
  assert.deepEqual(portsOf({ id: "a", kind: "stop", config: {} }), []);
  assert.deepEqual(portsOf({ id: "a", kind: "message", config: {} }), ["default"]);
});

test("validator never throws on hostile input", () => {
  for (const v of [null, 1, "x", [], {}, { steps: 5 }, { schema: "nurtuo.flow/v1", steps: [null, 3], edges: [] }]) {
    assert.doesNotThrow(() => validateFlow(v));
  }
});
