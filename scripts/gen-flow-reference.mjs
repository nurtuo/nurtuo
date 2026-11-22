#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Generates docs/flow-reference.md from sdk/ts/flow-schema/schema/flow.v1.schema.json and the
// example flows in examples/flows/reference/<kind>.flow.json.
//
//   node scripts/gen-flow-reference.mjs            write docs/flow-reference.md
//   node scripts/gen-flow-reference.mjs --check    exit 1 if the file is stale (CI-able)
//   node scripts/gen-flow-reference.mjs --validate also validate every example with the built
//                                                  @nurtuo/flow-validate (needs `pnpm --filter
//                                                  @nurtuo/flow-validate build` first); every example
//                                                  must have no errors/warnings and contain its kind
//
// Only Node built-ins are used. Field tables come from the schema; the prose in NOTES is
// hand-written and verified against the code (see the "Runtime status" lines).
import { readFileSync, writeFileSync, readdirSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = (p) => fileURLToPath(new URL(`../${p}`, import.meta.url));
const schema = JSON.parse(readFileSync(root("sdk/ts/flow-schema/schema/flow.v1.schema.json"), "utf8"));
const OUT = root("docs/flow-reference.md");
const args = new Set(process.argv.slice(2));

const kinds = schema.$defs.step.properties.kind.enum;

/** Output ports per kind (kept in sync with portsOf in flow-validate; --validate checks examples). */
const PORTS = {
  start: "`default`", connector: "`default`", queue: "`default`", map: "`default`", script: "`default`", message: "`default`",
  setProperties: "`default`", decision: "`true`, `false`", branch: "`1` .. `paths`",
  route: "one per route `id`, then `default`", tryCatch: "`try`, `catch`", processCall: "`default`",
  notify: "`default`", stop: "none (terminal)", returnDocuments: "none (terminal)",
};

/** Hand-written, code-verified notes per kind. */
const NOTES = {
  start: [
    "A flow must have exactly one `start` step and it cannot have incoming edges.",
    "Runtime status: only `listener` with `connector` `http` and `queue` start steps are bound to push listeners by the runtime (`core/runtime/internal/listener`). `manual` and `schedule` are accepted by the schema, but nothing in the runtime fires a cron schedule yet, and other connector listeners (bridge listeners) are not built by the listener factory.",
    "The HTTP listener matches `path` exactly (it must start with `/`; there is no `{id}` path templating despite the `/employees/{id}` shape in `examples/flows/get-employee.flow.json`), defaults `method` to `POST`, and accepts bodies up to 1 MiB.",
    "HTTP listener protection is opt-in: `auth` (`bearer`, `apiKey` or `hmac`; the secret is resolved from the runtime secret store and compared in constant time; a failed check is answered `401` before the flow runs) and `tls` (PEM certificate and key from secrets, TLS 1.2 minimum). The runtime refuses to start an HTTP listener on a non-loopback `http_addr` without `auth` unless `insecure` is `true`. `auth`, `tls` and `insecure` are ignored by other start types. See [connectors/http.md](connectors/http.md#http-listener-start-step).",
  ],
  connector: [
    "`operation` is an operation name of the connector. The engine maps `GET`, `QUERY`, `CREATE`, `UPDATE`, `UPSERT` and `DELETE` (case-insensitive) to the matching contract operation type; any other value is sent as `EXECUTE` with that value as the custom type.",
    "`connection` selects the connection registered as `<connector>/<connection>`; if that is not registered the entry registered as `<connector>` is used.",
    "Per-document results: `SUCCESS` passes the document on (replaced by output documents if the connector returned any), `APPLICATION_ERROR` and `FAILURE` mark the document failed. Tracked values appear as document properties `tracked.<name>`.",
    "Runtime status: the `nurtuo` binary registers the native connectors `http`, `sftp`, `postgres` and `nats`. The `connection` is looked up among the connections of the deployed bundle; `secret://name` values in it are resolved from the runtime secret store, and the connection is tested at deploy time (`connection_test`). Bridge connectors cannot be configured from `runtime.yaml` yet. Verified with unit and end-to-end tests of the http connector, not against every external system.",
    "`postgres` takes the SQL from the input document (`{\"sql\": \"...\", \"params\": [...]}`), so a flow normally builds that document with a `map` or `message` step before a `postgres` `QUERY` step; `examples/flows/get-employee.flow.json` shows the shape of the flow but omits that step.",
    "The HTTP connector reads its per-document overrides from `http.method`, `http.path`, `http.query.*` and `http.header.*`. The HTTP listener sets `http.request.method`, `http.request.path`, `http.request.header.*` (lowercased) and `http.request.query.*` instead, so incoming request metadata is never mistaken for an override; copy what you need with `setProperties`.",
  ],
  queue: [
    "Runtime status: the schema accepts `queue` steps, but the engine has no step runner for them yet; a document reaching this step fails with code `no_runner`. Publishing to a queue from a flow is not implemented. (Starting a flow from a queue works, see `start`.)",
  ],
  script: [
    "Runs a WebAssembly module over every document in a sandbox (memory, time and output limits, capability allow-list). `script` names a registered module; `timeoutMs` (default 10000), `memoryMiB` (default 32) and `capabilities` (`clock`, `random`) are optional. Zero output documents drops the document, several replace it. A failing document gets code `script.<code>`; other documents continue. Details in [scripts.md](scripts.md).",
    "Runtime status: the runner is registered in the `nurtuo` binary, but modules are not yet delivered to the runtime (bundles do not carry them), so a `script` step on a deployed runtime currently fails with `script is not registered`. The step works in the engine with an in-process module registry (see the tests).",
  ],
  map: [
    "`jsonata` is a JSONata expression evaluated against the JSON payload of each document; the JSON result replaces the payload. An undefined result drops the document. A payload that is not JSON fails the document.",
  ],
  message: [
    "`template` replaces the document payload. `{{operand}}` placeholders are substituted; operands are `doc.id`, `doc.props.NAME`, `process.props.NAME`, string literals and numbers. A missing property substitutes the empty string.",
  ],
  setProperties: [
    "The schema description says the values are CEL expressions. In the engine each `value` is rendered with the same `{{operand}}` template mechanism as `message` (a plain string is stored as is).",
    "`scope` is `document` (default) or `process`. Process properties are shared by all documents of one execution. A name may be set only once per scope in one step (`setProperties.duplicate_name`).",
  ],
  decision: [
    "Documents for which the condition is true leave by `true`, others by `false`. A condition error fails that document.",
    "The default evaluator (`BasicEvaluator`, used by the runtime deployer) supports only `true`/`false`, `!expr`, and `<operand> <op> <operand>` with `== != < <= > >=`; operands are string or number literals, `doc.id`, `doc.props.NAME`, `process.props.NAME`. Missing properties read as the empty string. Full CEL exists as `engine.CELEvaluator` (variables `doc.id`, `doc.props`, `process.props`) but is not selected by the runtime yet.",
  ],
  branch: [
    "Every document set is copied to each path; paths run sequentially in order `1`..`paths`, not in parallel. Allowed range `paths`: 2 to 25.",
  ],
  route: [
    "Each document goes to the first route whose condition is true, otherwise to `default`. Route ids must be unique and must not be `default` (`route.duplicate_id`, `route.reserved_id`). Conditions use the same evaluator as `decision`.",
  ],
  tryCatch: [
    "Documents that fail anywhere on the `try` path (before the path ends) are retried up to `retries` times from their state at entry to the step, then sent down `catch` with the error in document properties `nurtuo.error.message`, `nurtuo.error.step` and `nurtuo.error.code`. There is no delay between retries.",
  ],
  processCall: [
    "Runs another flow with the current documents; returned documents continue on `default`. Process properties are copied into the called flow and not copied back. Nesting is limited to depth 16 (`processCall.depth` failure). A flow cannot call itself (`processCall.self_reference`); that the called flow exists is not checked by the validator and is resolved at run time.",
  ],
  notify: [
    "Runtime status: the schema accepts `notify` steps, but the engine has no step runner for them yet; a document reaching this step fails with code `no_runner`.",
  ],
  stop: [
    "Ends the whole execution. When the flow was started by the HTTP listener, `status` and `message` become the HTTP response; without `status` the listener answers with the returned documents (or 204).",
  ],
  returnDocuments: [
    "Hands the documents that reach it to the caller (for the HTTP listener: the response body; one document is written as is, several as a JSON array).",
  ],
};

const FALLBACK = { tryCatch: "Runs a path and handles failures on a second path." };
const summary = (k) => schema.$defs[`${k}Config`].description.replace(/\s*(Ports:|Terminal:).*$/, "").trim() || FALLBACK[k];
const esc = (s) => String(s).replaceAll("|", "\\|");
const code = (s) => "`" + esc(s) + "`";

/** Resolves a local `#/$defs/<name>` reference (one level). */
function deref(p) {
  return p?.$ref ? schema.$defs[p.$ref.split("/").pop()] : p;
}
function typeOf(p) {
  if (p.$ref) return `${deref(p).type ?? "any"} (${p.$ref.split("/").pop()})`;
  if (p.const !== undefined) return `const ${code(JSON.stringify(p.const))}`;
  if (p.enum) return "enum";
  if (p.type === "array") return `array of ${p.items?.$ref ? p.items.$ref.split("/").pop() : (p.items?.type ?? "any")}`;
  return p.type ?? "any";
}
function constraints(p) {
  p = deref(p);
  const c = [];
  if (p.enum) c.push("one of " + p.enum.map((v) => code(v)).join(", "));
  if (p.minLength !== undefined) c.push(`minLength ${p.minLength}`);
  if (p.minimum !== undefined) c.push(`>= ${p.minimum}`);
  if (p.maximum !== undefined) c.push(`<= ${p.maximum}`);
  if (p.minItems !== undefined) c.push(`minItems ${p.minItems}`);
  if (p.pattern) c.push(`pattern ${code(p.pattern)}`);
  return c.join("; ");
}

function table(def, indent = "") {
  const req = new Set(def.required ?? []);
  const props = Object.entries(def.properties ?? {});
  if (props.length === 0) return `${indent}No fields. \`config\` must be \`{}\`.\n`;
  let out = `${indent}| Field | Type | Required | Constraints |\n${indent}|---|---|---|---|\n`;
  for (const [name, p] of props) {
    out += `${indent}| ${code(name)} | ${typeOf(p)} | ${req.has(name) ? "yes" : "no"} | ${constraints(p)} |\n`;
  }
  return out;
}

function conditional(def) {
  const rules = [];
  for (const r of def.allOf ?? []) {
    const when = r.if?.properties && Object.entries(r.if.properties)[0];
    if (when && r.then?.required) {
      rules.push(`When ${code(when[0])} is ${code(JSON.stringify(when[1].const))}: required ${r.then.required.map((x) => code(x)).join(", ")}.`);
    }
  }
  return rules;
}

function example(kind) {
  const raw = readFileSync(root(`examples/flows/reference/${kind}.flow.json`), "utf8");
  return raw.replace(/\r\n/g, "\n").trimEnd();
}

function render() {
  const o = [];
  o.push("<!-- SPDX-License-Identifier: Apache-2.0 -->");
  o.push("<!-- GENERATED by scripts/gen-flow-reference.mjs from sdk/ts/flow-schema/schema/flow.v1.schema.json. Do not edit; run `node scripts/gen-flow-reference.mjs`. -->");
  o.push("# Flow reference (`nurtuo.flow/v1`)\n");
  o.push("This page is generated from the JSON Schema (`sdk/ts/flow-schema/schema/flow.v1.schema.json`) and the example flows in `examples/flows/reference/`. Field names, types and constraints are the schema's; semantic rules (ports, cycles, ...) are in [flow-validation.md](flow-validation.md). Read [concepts.md](concepts.md) first if flows, steps and ports are new to you.\n");
  o.push("Each example below is a complete flow that passes the validator (`nurtuoctl validate`); `node scripts/gen-flow-reference.mjs --validate` re-checks them.\n");
  o.push("## Flow document\n");
  o.push(table(schema));
  o.push(`Root additional properties are not allowed (\`additionalProperties: false\`). ${schema.description}\n`);
  o.push(`\`edges\` keys must match ${code(schema.properties.edges.propertyNames.pattern)}: the step id, a colon and the port name. Values are the id of the target step.\n`);
  o.push("## Step envelope\n");
  o.push(table(schema.$defs.step));
  o.push(`Step ids must match ${code(schema.$defs.step.properties.id.pattern)}. \`config\` is validated against the per-kind schema below; unknown fields are rejected in every config.\n`);
  o.push("## Step kinds\n");
  o.push("| Kind | Output ports | Summary |\n|---|---|---|");
  for (const k of kinds) o.push(`| [${k}](#${k.toLowerCase()}) | ${PORTS[k]} | ${esc(summary(k))} |`);
  o.push("");
  for (const k of kinds) {
    const def = schema.$defs[`${k}Config`];
    o.push(`### ${k}\n`);
    o.push(`${summary(k)}\n`);
    o.push(`Output ports: ${PORTS[k]}.\n`);
    o.push(table(def));
    for (const [name, p] of Object.entries(def.properties ?? {})) {
      if (p.type === "array" && p.items?.type === "object") {
        o.push(`Each element of ${code(name)}:\n`);
        o.push(table(p.items));
      }
      const d = deref(p);
      if (p.$ref && d.type === "object" && d.properties) {
        o.push(`${code(name)}${d.description ? ": " + esc(d.description) : ""}\n`);
        o.push(table(d));
      }
    }
    for (const r of conditional(def)) o.push(`- ${r}`);
    for (const n of NOTES[k] ?? []) o.push(`- ${n}`);
    o.push("");
    o.push("Example (`examples/flows/reference/" + k + ".flow.json`):\n");
    o.push("```json\n" + example(k) + "\n```\n");
  }
  return o.join("\n").replace(/\n{3,}/g, "\n\n") + "\n";
}

const text = render();

if (args.has("--validate")) {
  const mod = await import(pathToFileURL(root("sdk/ts/flow-validate/dist/index.js")).href);
  let bad = 0;
  for (const f of readdirSync(root("examples/flows/reference"))) {
    const flow = JSON.parse(readFileSync(root(`examples/flows/reference/${f}`), "utf8"));
    const diags = mod.validateFlow(flow);
    for (const d of diags) { console.error(`${f}: ${d.severity} ${d.code} ${d.path}`); if (d.severity === "error" || d.severity === "warning") bad++; }
  }
  for (const k of kinds) {
    const flow = JSON.parse(readFileSync(root(`examples/flows/reference/${k}.flow.json`), "utf8"));
    if (!flow.steps.some((s) => s.kind === k)) { console.error(`${k}: example does not contain a ${k} step`); bad++; }
  }
  if (bad) process.exit(1);
  console.log(`validated ${kinds.length} examples: no errors or warnings`);
}

if (args.has("--check")) {
  let have = "";
  try { have = readFileSync(OUT, "utf8"); } catch { /* missing */ }
  if (have.replace(/\r\n/g, "\n") !== text) { console.error("docs/flow-reference.md is stale: run node scripts/gen-flow-reference.mjs"); process.exit(1); }
  console.log("docs/flow-reference.md is up to date");
} else if (!args.has("--validate") || args.has("--write")) {
  writeFileSync(OUT, text);
  console.log(`wrote ${OUT}`);
}
