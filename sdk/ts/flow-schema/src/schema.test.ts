// SPDX-License-Identifier: Apache-2.0
// Keeps the three representations of the format consistent: the JSON Schema file (source of
// truth), the generated TS module, and the Go copy used by sdk/go/flow.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { createRequire } from "node:module";
import { flowSchema, STEP_KINDS, FLOW_SCHEMA_VERSION } from "./index.js";

const schemaUrl = new URL("../schema/flow.v1.schema.json", import.meta.url);
const goCopy = new URL("../../../go/flow/flow.v1.schema.json", import.meta.url);
const corpus = new URL("../testdata/conformance/", import.meta.url);

test("generated TS schema module equals schema/flow.v1.schema.json", () => {
  assert.deepEqual(flowSchema, JSON.parse(readFileSync(schemaUrl, "utf8")));
});

test("Go copy of the schema is byte-identical (run `pnpm run sync` to refresh)", () => {
  assert.equal(readFileSync(goCopy, "utf8"), readFileSync(schemaUrl, "utf8"));
});

test("STEP_KINDS and the version const match the schema", () => {
  const s = JSON.parse(readFileSync(schemaUrl, "utf8"));
  assert.deepEqual([...STEP_KINDS], s.$defs.step.properties.kind.enum);
  assert.equal(s.properties.schema.const, FLOW_SCHEMA_VERSION);
});

test("schema is JSON Schema 2020-12 and every kind has a config definition", () => {
  const s = JSON.parse(readFileSync(schemaUrl, "utf8"));
  assert.equal(s.$schema, "https://json-schema.org/draft/2020-12/schema");
  for (const k of STEP_KINDS) assert.ok(s.$defs[`${k}Config`], `missing $defs.${k}Config`);
});

// Cross-check against a real JSON Schema implementation (Ajv, dev dependency only): for the
// corpus, "schema.* diagnostics exist" must equal "Ajv says invalid".
test("Ajv (2020-12) agrees with the corpus on structural validity", async () => {
  const require = createRequire(import.meta.url);
  const Ajv2020 = require("ajv/dist/2020").default as new (o: object) => {
    compile: (s: object) => ((d: unknown) => boolean);
  };
  const validate = new Ajv2020({ strict: false, allErrors: true }).compile(JSON.parse(readFileSync(schemaUrl, "utf8")));
  let checked = 0;
  for (const dir of ["valid", "invalid"]) {
    for (const f of readdirSync(new URL(`${dir}/`, corpus)).filter((n) => n.endsWith(".json"))) {
      const c = JSON.parse(readFileSync(new URL(`${dir}/${f}`, corpus), "utf8")) as { flow: unknown; expect: { code: string }[] };
      const structural = c.expect.some((e) => e.code.startsWith("schema."));
      assert.equal(!validate(c.flow), structural, `${dir}/${f}`);
      checked++;
    }
  }
  assert.ok(checked > 40);
});
