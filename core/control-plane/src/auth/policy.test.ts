// SPDX-License-Identifier: AGPL-3.0-only
// Guard rail: every route of the API must have an explicit authorization decision.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { POLICIES, matchPolicy } from "./policy.js";
import { INSTANCE_PERMISSIONS } from "./instance.js";
import { isPermission } from "./permissions.js";
import { j, setup } from "./test-util.js";

interface Op { method: string; path: string; block: string }
/** Minimal line-based reader for openapi.yaml: `  /path:` then `    method:` blocks. */
function openapiOps(): Op[] {
  const lines = readFileSync(fileURLToPath(new URL("../../openapi.yaml", import.meta.url)), "utf8").split(/\r?\n/);
  const ops: Op[] = [];
  let path = "", cur: Op | undefined, inPaths = false;
  for (const l of lines) {
    if (/^\S/.test(l)) { inPaths = l.startsWith("paths:"); cur = undefined; continue; }
    if (!inPaths) continue;
    const p = /^  (\/\S+):\s*$/.exec(l);
    if (p) { path = p[1]!; cur = undefined; continue; }
    const m = /^    (get|post|put|patch|delete):(.*)$/.exec(l);
    if (m) { cur = { method: m[1]!.toUpperCase(), path, block: m[2]! }; ops.push(cur); continue; }
    if (cur) cur.block += `\n${l}`;
  }
  return ops;
}
const concrete = (path: string) => path.replace(/\{(\w+)\}/g, (_, n) => (n === "number" ? "1" : `${n}-x`));
const toPattern = (path: string) => path.replace(/\{(\w+)\}/g, ":$1");

test("openapi.yaml is parseable and non-trivial", () => { assert.ok(openapiOps().length >= 40); });

test("every openapi operation has an authz policy, and every policy is documented", () => {
  const ops = openapiOps();
  const documented = new Set(ops.map((o) => `${o.method} ${toPattern(o.path)}`));
  const policies = new Set(POLICIES.map((p) => `${p.method} ${p.pattern.replace(/:(ws|id|userId|tokenId|number)\b/g, (_, n) => `:${n}`)}`));
  for (const o of ops) {
    const hit = matchPolicy(o.method, concrete(o.path));
    assert.ok(hit, `no authorization policy for ${o.method} ${o.path} (add it to src/auth/policy.ts)`);
  }
  for (const p of POLICIES) assert.ok(documented.has(`${p.method} ${p.pattern}`), `policy ${p.method} ${p.pattern} is missing from openapi.yaml`);
  assert.equal(documented.size, policies.size);
  assert.equal(new Set(POLICIES.map((p) => `${p.method} ${p.pattern}`)).size, POLICIES.length, "duplicate policies");
});

test("policy table is well-formed", () => {
  for (const p of POLICIES) {
    if (p.access === "permission") { assert.ok(p.permission && isPermission(p.permission), `${p.action} needs a valid permission`); assert.notEqual(p.workspace, "none", `${p.action} must be workspace scoped`); }
    else assert.equal(p.permission, undefined);
    if (p.access === "instance") { assert.ok(p.instancePermission && (INSTANCE_PERMISSIONS as readonly string[]).includes(p.instancePermission), `${p.action} needs a valid instancePermission`); assert.equal(p.workspace, "none", `${p.action} must not be workspace scoped`); }
    else assert.equal(p.instancePermission, undefined, `${p.action}: instancePermission only with access "instance"`);
    if (p.workspace === "path") assert.ok(p.pattern.includes(":ws"), p.pattern);
  }
  const actions = POLICIES.map((p) => p.action);
  assert.equal(new Set(actions).size, actions.length, "action names must be unique (they key the audit log and handlers)");
  // only the documented set of routes is reachable without credentials
  const pub = POLICIES.filter((p) => p.access === "public").map((p) => p.action).sort();
  assert.deepEqual(pub, ["auth.login", "auth.oidc_callback", "auth.oidc_login", "auth.saml_acs", "auth.saml_login", "auth.saml_metadata", "scim.groups.create", "scim.groups.list", "scim.users.create", "scim.users.delete", "scim.users.get", "scim.users.list", "scim.users.patch", "scim.users.replace"]);
});

test("openapi declares security, 401 and 403 for every operation", () => {
  for (const o of openapiOps()) {
    const pol = matchPolicy(o.method, concrete(o.path))!.policy;
    if (pol.access === "public") assert.match(o.block, /security: \[\]/, `${o.method} ${o.path} is public and must say security: []`);
    else assert.doesNotMatch(o.block, /security: \[\]/, `${o.method} ${o.path} must not opt out of security`);
    if (pol.access === "public") continue; // public ops may only fail with 401/429/501 as documented per operation
    assert.match(o.block, /"401"/, `${o.method} ${o.path} lacks a 401 response`);
    assert.match(o.block, /"403"/, `${o.method} ${o.path} lacks a 403 response`);
  }
});

test("runtime: every non-public documented route rejects anonymous callers with 401 and unknown routes are denied", async () => {
  const { call } = setup();
  for (const o of openapiOps()) {
    const pol = matchPolicy(o.method, concrete(o.path))!.policy;
    const r = await call(o.method, concrete(o.path), { body: {} });
    if (pol.access === "public") assert.notEqual(r.status, 403, `${o.method} ${o.path}`);
    else assert.equal(r.status, 401, `${o.method} ${o.path} -> ${r.status}`);
  }
  assert.equal((await call("GET", "/v1/definitely-not-a-route")).status, 401);
  assert.equal((await call("GET", "/")).status, 401);
});

test("runtime: a route without a policy is 403 even for an owner (fail closed)", async () => {
  const { call, owner, mint } = setup();
  const { user, ws } = await owner("o@x.io", "w1");
  const t = await mint(ws.id, user.id, ["component:read"]);
  const r = await call("GET", "/v1/not-a-route", { token: t.raw });
  assert.equal(r.status, 403);
  assert.equal(j(r).error.code, "no_policy");
  assert.equal((await call("GET", "/v1/components", { token: t.raw })).status, 200);
});
