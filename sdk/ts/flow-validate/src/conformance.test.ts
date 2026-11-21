// SPDX-License-Identifier: Apache-2.0
// Runs the shared conformance corpus (sdk/ts/flow-schema/testdata/conformance). The Go
// validator (sdk/go/flow) runs the exact same files; both must produce identical diagnostics.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { validateFlow } from "./index.js";

const corpus = new URL("../../flow-schema/testdata/conformance/", import.meta.url);
const examples = new URL("../../../../examples/flows/", import.meta.url);

type D = { severity: string; code: string; path: string; stepId?: string };
interface Case {
  description: string;
  flow: unknown;
  expect: D[];
}

const project = (d: D) => ({ severity: d.severity, code: d.code, path: d.path, stepId: d.stepId ?? "" });

for (const dir of ["valid", "invalid"]) {
  const files = readdirSync(new URL(`${dir}/`, corpus)).filter((f) => f.endsWith(".json")).sort();
  test(`corpus ${dir} is not empty`, () => assert.ok(files.length > 0));
  for (const f of files) {
    test(`conformance ${dir}/${f}`, () => {
      const c = JSON.parse(readFileSync(new URL(`${dir}/${f}`, corpus), "utf8")) as Case;
      const got = validateFlow(c.flow).map(project);
      assert.deepEqual(got, c.expect.map(project));
      const hasError = got.some((d) => d.severity === "error");
      assert.equal(hasError, dir === "invalid");
    });
  }
}

test("every example flow has no errors", () => {
  for (const f of readdirSync(examples).filter((n) => n.endsWith(".flow.json"))) {
    const flow = JSON.parse(readFileSync(new URL(f, examples), "utf8"));
    const errors = validateFlow(flow).filter((d) => d.severity === "error");
    assert.deepEqual(errors, [], f);
  }
});
