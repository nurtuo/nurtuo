// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { test } from "node:test";
import { parseArgs, UsageError } from "./args.js";
import { configPath } from "./config.js";
import { diffFlows, formatFlow, slugify } from "./flowtools.js";

const specs = {
  json: { type: "boolean" as const, description: "" },
  output: { type: "string" as const, short: "o", description: "" },
  write: { type: "boolean" as const, short: "w", description: "" },
};

test("parseArgs: long, short, =, attached short value, boolean cluster, --", () => {
  const p = parseArgs(["a", "--json", "--output=x", "-w", "b", "-o", "y", "--", "--json", "-z"], specs);
  assert.deepEqual(p.positionals, ["a", "b", "--json", "-z"]);
  assert.deepEqual(p.flags, { json: true, output: "y", write: true });
  assert.equal(parseArgs(["-ofile"], specs).flags["output"], "file");
  assert.equal(parseArgs(["-wo", "f"], specs).flags["output"], "f");
  assert.deepEqual(parseArgs(["-", "-5"], specs).positionals, ["-", "-5"]);
  assert.equal(parseArgs(["--json=false"], specs).flags["json"], false);
});

test("parseArgs errors", () => {
  assert.throws(() => parseArgs(["--nope"], specs), UsageError);
  assert.throws(() => parseArgs(["-q"], specs), UsageError);
  assert.throws(() => parseArgs(["--output"], specs), /requires a value/);
  assert.throws(() => parseArgs(["--json=maybe"], specs), /does not take a value/);
});

test("configPath precedence and platform defaults", () => {
  assert.equal(configPath({ NURTUO_CONFIG: "/x/c.json" }, "linux"), "/x/c.json");
  assert.match(configPath({ XDG_CONFIG_HOME: "/xdg" }, "linux").replace(/\\/g, "/"), /^\/xdg\/nurtuo\/config\.json$/);
  assert.match(configPath({ APPDATA: "C:\\A" }, "win32").replace(/\\/g, "/"), /C:\/A\/nurtuo\/config\.json$/);
});

test("slugify", () => {
  assert.equal(slugify("My Flow!"), "my-flow");
  assert.equal(slugify("  --x_y.z "), "x_y.z");
  assert.equal(slugify("!!!"), "");
});

test("formatFlow is idempotent and ordering-stable", () => {
  const once = formatFlow({ edges: { "b:default": "c", "a:default": "b" }, steps: [{ config: { z: 1, a: { y: 1, x: 2 } }, id: "a", kind: "start" }], id: "f", schema: "nurtuo.flow/v1", extra: 1 });
  assert.equal(formatFlow(JSON.parse(once)), once);
  assert.ok(once.indexOf('"schema"') < once.indexOf('"id"'));
  assert.ok(once.indexOf('"a:default"') < once.indexOf('"b:default"'));
  assert.ok(once.indexOf('"x"') < once.indexOf('"y"'));
  assert.ok(once.indexOf('"edges"') < once.indexOf('"extra"'));
});

test("diffFlows: array config compared by index; missing sections tolerated", () => {
  const a = { steps: [{ id: "s", kind: "setProperties", config: { properties: [{ name: "a", value: "1" }] } }], edges: {} };
  const b = { steps: [{ id: "s", kind: "setProperties", config: { properties: [{ name: "a", value: "2" }] } }], edges: {} };
  const d = diffFlows(a, b);
  assert.deepEqual(d.steps.changed, [{ id: "s", changes: [{ path: "config.properties[0].value", from: "1", to: "2" }] }]);
  assert.equal(diffFlows({}, {}).identical, true);
  assert.equal(diffFlows(null, a).identical, false);
});
