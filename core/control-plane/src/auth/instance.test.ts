// SPDX-License-Identifier: AGPL-3.0-only
// Instance-level permissions (instance:workspace_create): config parsing and AuthService wiring.
import assert from "node:assert/strict";
import { test } from "node:test";
import { DEFAULT_INSTANCE_CONFIG, INSTANCE_PERMISSIONS, InstanceConfigError, instancePermissionsFor, parseInstanceConfig } from "./instance.js";
import { ALL_PERMISSIONS, isPermission } from "./permissions.js";
import { adminsConfig, j, setup } from "./test-util.js";
import type { Principal } from "./types.js";

const WC = "instance:workspace_create";

test("instance config: defaults to admins with an empty set", () => {
  for (const env of [{}, { NURTUO_INSTANCE_ADMINS: "", NURTUO_ALLOW_WORKSPACE_CREATE: "", NURTUO_BOOTSTRAP_EMAIL: "" }, { NURTUO_INSTANCE_ADMINS: " , ,", NURTUO_BOOTSTRAP_EMAIL: "  " }]) {
    const c = parseInstanceConfig(env);
    assert.equal(c.workspaceCreate, "admins");
    assert.equal(c.admins.size, 0);
  }
  assert.equal(DEFAULT_INSTANCE_CONFIG.workspaceCreate, "admins");
  assert.equal(DEFAULT_INSTANCE_CONFIG.admins.size, 0);
});

test("instance config: parses, trims, lower-cases and de-duplicates NURTUO_INSTANCE_ADMINS; ignores empty items", () => {
  const c = parseInstanceConfig({ NURTUO_INSTANCE_ADMINS: " Ops@Corp.Example ,, dev@corp.example,ops@corp.example, ", NURTUO_ALLOW_WORKSPACE_CREATE: " ADMINS " });
  assert.deepEqual([...c.admins].sort(), ["dev@corp.example", "ops@corp.example"]);
  assert.equal(c.workspaceCreate, "admins");
  assert.equal(parseInstanceConfig({ NURTUO_ALLOW_WORKSPACE_CREATE: "Any" }).workspaceCreate, "any");
});

test("instance config: bootstrap email is an instance admin; malformed bootstrap email throws", () => {
  const c = parseInstanceConfig({ NURTUO_BOOTSTRAP_EMAIL: " Admin@Nurtuo.Local " });
  assert.deepEqual([...c.admins], ["admin@nurtuo.local"]);
  const both = parseInstanceConfig({ NURTUO_BOOTSTRAP_EMAIL: "admin@nurtuo.local", NURTUO_INSTANCE_ADMINS: "ops@corp.example,ADMIN@nurtuo.local" });
  assert.deepEqual([...both.admins].sort(), ["admin@nurtuo.local", "ops@corp.example"]);
  assert.throws(() => parseInstanceConfig({ NURTUO_BOOTSTRAP_EMAIL: "not-an-email" }), (e: unknown) => e instanceof InstanceConfigError && /NURTUO_BOOTSTRAP_EMAIL/.test(e.message));
});

test("instance config: malformed admin item throws with the item position; unknown mode throws", () => {
  assert.throws(() => parseInstanceConfig({ NURTUO_INSTANCE_ADMINS: "ops@corp.example,not-an-email" }),
    (e: unknown) => e instanceof InstanceConfigError && e.message === "NURTUO_INSTANCE_ADMINS item 2 is not a valid email");
  assert.throws(() => parseInstanceConfig({ NURTUO_INSTANCE_ADMINS: "a@b.io, ,a b@c.io" }), /NURTUO_INSTANCE_ADMINS item 2 is not a valid email/, "empty items do not count");
  assert.throws(() => parseInstanceConfig({ NURTUO_INSTANCE_ADMINS: `${"x".repeat(250)}@c.io` }), /item 1/);
  for (const m of ["yes", "all", "admin", "1"]) {
    assert.throws(() => parseInstanceConfig({ NURTUO_ALLOW_WORKSPACE_CREATE: m }), (e: unknown) => e instanceof InstanceConfigError && e.message === 'NURTUO_ALLOW_WORKSPACE_CREATE must be "admins" or "any"');
  }
});

test("instance config: mode any grants instance:workspace_create to every email; admins only to listed ones", () => {
  const admins = parseInstanceConfig({ NURTUO_INSTANCE_ADMINS: "ops@corp.example" });
  assert.ok(instancePermissionsFor("ops@corp.example", admins).has(WC));
  assert.ok(instancePermissionsFor("OPS@corp.example", admins).has(WC));
  assert.equal(instancePermissionsFor("viewer@corp.example", admins).size, 0);
  assert.equal(instancePermissionsFor(undefined, admins).size, 0);
  const any = parseInstanceConfig({ NURTUO_ALLOW_WORKSPACE_CREATE: "any" });
  assert.ok(instancePermissionsFor("viewer@corp.example", any).has(WC));
  assert.ok(instancePermissionsFor(undefined, any).has(WC));
});

test("instance permissions: tokens never hold instance permissions; disabled users are rejected before the check", async () => {
  // mode any: even then a token holds nothing
  for (const instance of [adminsConfig("o@x.io"), { ...adminsConfig("o@x.io"), workspaceCreate: "any" as const }]) {
    const { service, authRepo, owner, member, login } = setup({ service: { instance } });
    const { ws } = await owner("o@x.io", "ws-a");
    const t = await member(ws.id, "o@x.io", "owner");
    const tokPrincipal = (await service.authenticate({ authorization: `Bearer ${t.token.raw}` }))!;
    assert.equal(tokPrincipal.kind, "token");
    assert.equal((await service.instancePermissions(tokPrincipal)).size, 0);

    const s = await login("o@x.io");
    const userPrincipal = (await service.authenticate({ cookie: `__Host-nurtuo_session=${s.cookie}` }))!;
    assert.ok((await service.instancePermissions(userPrincipal)).has(WC));

    const u = (await authRepo.getUserByEmail("o@x.io"))!;
    await authRepo.setUserStatus(u.id, "disabled");
    await assert.rejects(service.authenticate({ cookie: `__Host-nurtuo_session=${s.cookie}` }), (e: any) => e.status === 401);
  }
  // service without an instance option fails closed
  const { service, owner } = setup();
  const { user } = await owner("o@x.io", "ws-a");
  const p: Principal = { kind: "user", userId: user.id, tokenWorkspaceId: null, tokenScopes: null, memberships: new Map(), viaCookie: false };
  assert.equal((await service.instancePermissions(p)).size, 0);
  assert.equal(service.isInstanceAdminEmail("o@x.io"), false);
});

test("instance permissions are not workspace permissions: not in ALL_PERMISSIONS, not a token scope, not in /v1/roles", async () => {
  for (const ip of INSTANCE_PERMISSIONS) {
    assert.equal((ALL_PERMISSIONS as readonly string[]).includes(ip), false);
    assert.equal(isPermission(ip), false);
  }
  const { call, owner, member } = setup({ service: { instance: adminsConfig("o@x.io") } });
  const { ws } = await owner("o@x.io", "ws-a");
  const t = await member(ws.id, "o@x.io", "owner");
  const roles = j(await call("GET", "/v1/roles", { token: t.token.raw }));
  assert.equal(roles.permissions.includes(WC), false);
  for (const r of roles.roles) assert.equal(r.permissions.includes(WC), false, r.name);
  const mint = await call("POST", `/v1/workspaces/${ws.id}/tokens`, { token: t.token.raw, body: { name: "k", scopes: [WC] } });
  assert.equal(mint.status, 400);
  assert.match(j(mint).error.message, /unknown scope/);
});
