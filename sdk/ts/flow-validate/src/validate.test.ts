// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import type { Flow } from "@nurtuo/flow-schema";
import { validateFlow } from "./index.js";

const example = JSON.parse(
  readFileSync(new URL("../../../../examples/flows/get-employee.flow.json", import.meta.url), "utf8"),
) as Flow;

test("GetEmployee example is valid", () => {
  assert.deepEqual(validateFlow(example), []);
});

test("edge to unknown step is reported", () => {
  const broken: Flow = { ...example, edges: { ...example.edges, "return:default": "nowhere" } };
  assert.ok(validateFlow(broken).some((i) => i.code === "edge_target"));
});
