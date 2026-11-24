// SPDX-License-Identifier: AGPL-3.0-only
// Role x route matrix, privilege escalation, cross-workspace isolation.
import assert from "node:assert/strict";
import { test } from "node:test";
import { ROLES, ROLE_PERMISSIONS, type Role } from "./permissions.js";
import { POLICIES } from "./policy.js";
import { PASSWORD, j, setup } from "./test-util.js";

const ALL: Role[] = ["owner", "admin", "editor", "viewer", "deployer", "runtime"];
// Independent, hand-written expectation (NOT derived from permissions.ts) of who may call what.
const EXPECT: Record<string, Role[]> = {
  "component.list": ALL, "component.get": ALL, "revision.list": ALL, "revision.get": ALL,
  "component.create": ["owner", "admin", "editor"], "component.update": ["owner", "admin", "editor"], "revision.create": ["owner", "admin", "editor"],
  "component.delete": ["owner", "admin", "editor"],
  "runtime.list": ALL, "runtime.get": ALL, "desired_state.get": ALL,
  "runtime.register": ["owner", "admin", "runtime"],
  "deployment.list": ALL, "deployment.get": ALL,
  "deployment.create": ["owner", "admin", "deployer"],
  "deployment.undeploy": ["owner", "admin", "deployer"],
  "deployment.report_status": ["owner", "admin", "runtime"],
  "workspace.get": ["owner", "admin", "editor", "viewer", "deployer"],
  "workspace.update": ["owner", "admin"], "workspace.delete": ["owner"],
  "member.list": ["owner", "admin", "editor", "viewer", "deployer"],
  "member.add": ["owner", "admin"], "member.update_role": ["owner", "admin"], "member.remove": ["owner", "admin"],
  "token.list": ["owner", "admin", "editor", "deployer"], "token.create": ["owner", "admin", "editor", "deployer"], "token.revoke": ["owner", "admin", "editor", "deployer"],
  "audit.query": ["owner", "admin"],
};

function concretePath(pattern: string, ws: string) {
  return pattern.replace(":ws", ws).replace(":number", "1").replace(/:(\w+)/g, "$1-x");
}
const bodyFor = (action: string) =>
  action === "member.add" ? { email: "new@x.io", role: "viewer", password: PASSWORD }
  : action === "member.update_role" ? { role: "viewer" }
  : action === "workspace.update" ? { name: "n" }
  : action === "token.create" ? { name: "n", scopes: ["component:read"] }
  : {};

test("role x route matrix: every permission-gated route, every role", async () => {
  const gated = POLICIES.filter((p) => p.access === "permission");
  assert.deepEqual(gated.map((p) => p.action).sort(), Object.keys(EXPECT).sort(), "matrix must cover exactly the gated routes");
  for (const role of ROLES) {
    for (const pol of gated) {
      // fresh world per call: destructive routes (workspace delete, member remove) must not affect the next one
      const { call, owner, member, login } = setup();
      const { ws } = await owner("owner@x.io", "ws-a");
      const { token } = await member(ws.id, `${role}@x.io`, role);
      // userOnly routes refuse API tokens for every role (checked below); drive them with a session.
      const sess = pol.userOnly ? await login(`${role}@x.io`) : undefined;
      const r = pol.userOnly
        ? await call(pol.method, concretePath(pol.pattern, ws.id), { cookie: sess!.cookie, csrf: sess!.csrf, body: bodyFor(pol.action) })
        : await call(pol.method, concretePath(pol.pattern, ws.id), { token: token.raw, body: bodyFor(pol.action), ws: pol.workspace === "context" ? ws.id : undefined });
      if (pol.userOnly) assert.equal((await call(pol.method, concretePath(pol.pattern, ws.id), { token: token.raw, body: bodyFor(pol.action) })).status, 403, `${role} token on userOnly ${pol.pattern}`);
      const allowed = EXPECT[pol.action]!.includes(role);
      if (allowed) assert.ok(r.status !== 403 && r.status !== 401, `${role} should reach ${pol.method} ${pol.pattern} but got ${r.status} ${JSON.stringify(r.body)}`);
      else assert.equal(r.status, 403, `${role} must be denied ${pol.method} ${pol.pattern}, got ${r.status}`);
    }
  }
});

test("session cookie principals follow the same matrix (spot check) and need CSRF to mutate", async () => {
  const { call, owner, member, login } = setup();
  const { ws } = await owner("o@x.io", "ws-a");
  await member(ws.id, "ed@x.io", "editor");
  await member(ws.id, "vw@x.io", "viewer");
  const ed = await login("ed@x.io"), vw = await login("vw@x.io");
  assert.equal(j(ed.r).user.email, "ed@x.io");
  assert.equal((await call("POST", "/v1/components", { cookie: ed.cookie, csrf: ed.csrf, body: { name: "c1" } })).status, 201);
  const noCsrf = await call("POST", "/v1/components", { cookie: ed.cookie, body: { name: "c2" } });
  assert.equal(noCsrf.status, 403); assert.equal(j(noCsrf).error.code, "csrf");
  assert.equal((await call("POST", "/v1/components", { cookie: ed.cookie, csrf: "wrong", body: { name: "c2" } })).status, 403);
  // someone else's csrf token does not work either
  assert.equal((await call("POST", "/v1/components", { cookie: ed.cookie, csrf: vw.csrf, body: { name: "c2" } })).status, 403);
  assert.equal((await call("GET", "/v1/components", { cookie: vw.cookie })).status, 200);
  assert.equal((await call("POST", "/v1/components", { cookie: vw.cookie, csrf: vw.csrf, body: { name: "c3" } })).status, 403);
  // logout revokes the session server-side
  assert.equal((await call("POST", "/v1/auth/logout", { cookie: ed.cookie, csrf: ed.csrf })).status, 204);
  assert.equal((await call("GET", "/v1/components", { cookie: ed.cookie })).status, 401);
});

test("session user with several workspaces must pick one; non-member header is 403", async () => {
  const { call, owner, member, login } = setup();
  const a = await owner("o@x.io", "ws-a");
  const b = await owner("o2@x.io", "ws-b");
  await member(b.ws.id, "o@x.io", "viewer");
  const s = await login("o@x.io");
  const r = await call("GET", "/v1/components", { cookie: s.cookie });
  assert.equal(r.status, 400); assert.equal(j(r).error.code, "workspace_required");
  assert.equal((await call("GET", "/v1/components", { cookie: s.cookie, ws: "ws-b" })).status, 200);
  assert.equal((await call("POST", "/v1/components", { cookie: s.cookie, csrf: s.csrf, ws: "ws-b", body: { name: "x" } })).status, 403, "viewer in B");
  assert.equal((await call("POST", "/v1/components", { cookie: s.cookie, csrf: s.csrf, ws: a.ws.id, body: { name: "x" } })).status, 201, "owner in A");
  assert.equal((await call("GET", "/v1/components", { cookie: s.cookie, ws: "ws-unknown" })).status, 403);
});

// ---- privilege escalation ------------------------------------------------------------------------
test("escalation: nobody can grant, change or remove a role above their own permissions", async () => {
  const { call, owner, member, authRepo } = setup();
  const { user: own, ws } = await owner("owner@x.io", "ws-a");
  const adm = await member(ws.id, "admin@x.io", "admin");
  const ed = await member(ws.id, "editor@x.io", "editor");
  const vw = await member(ws.id, "viewer@x.io", "viewer");
  const base = `/v1/workspaces/${ws.id}/members`;

  // editor tries to create an owner / admin / elevate themselves
  assert.equal((await call("POST", base, { token: ed.token.raw, body: { email: "evil@x.io", role: "owner", password: PASSWORD } })).status, 403);
  assert.equal((await call("PATCH", `${base}/${ed.user.id}`, { token: ed.token.raw, body: { role: "owner" } })).status, 403);
  assert.equal((await call("PATCH", `${base}/${ed.user.id}`, { token: ed.token.raw, body: { role: "admin" } })).status, 403);
  assert.equal((await authRepo.getMembership(ws.id, ed.user.id))!.role, "editor");
  assert.equal(await authRepo.getUserByEmail("evil@x.io"), undefined, "no account may be created by a denied request");

  // admin cannot mint owners, promote others to owner, or touch/remove owners
  const r1 = await call("POST", base, { token: adm.token.raw, body: { email: "evil@x.io", role: "owner", password: PASSWORD } });
  assert.equal(r1.status, 403); assert.equal(j(r1).error.code, "role_escalation");
  assert.equal(await authRepo.getUserByEmail("evil@x.io"), undefined);
  assert.equal((await call("PATCH", `${base}/${vw.user.id}`, { token: adm.token.raw, body: { role: "owner" } })).status, 403);
  assert.equal((await call("PATCH", `${base}/${own.id}`, { token: adm.token.raw, body: { role: "viewer" } })).status, 403, "admin demoting owner");
  assert.equal((await call("DELETE", `${base}/${own.id}`, { token: adm.token.raw })).status, 403, "admin removing owner");
  assert.equal((await call("POST", base, { token: adm.token.raw, body: { email: "owner@x.io", role: "viewer" } })).status, 403, "admin re-adding an owner as viewer (downgrade via add)");
  assert.equal((await authRepo.getMembership(ws.id, own.id))!.role, "owner");

  // admin may manage lower roles
  assert.equal((await call("PATCH", `${base}/${vw.user.id}`, { token: adm.token.raw, body: { role: "editor" } })).status, 200);
  assert.equal((await call("POST", base, { token: adm.token.raw, body: { email: "n@x.io", role: "deployer", password: PASSWORD } })).status, 201);

  // runtime/deployer cannot manage members at all
  const dep = await member(ws.id, "dep@x.io", "deployer");
  assert.equal((await call("POST", base, { token: dep.token.raw, body: { email: "z@x.io", role: "viewer", password: PASSWORD } })).status, 403);

  // unknown role values are rejected as bad input
  assert.equal((await call("PATCH", `${base}/${vw.user.id}`, { token: own && (await member(ws.id, "owner2@x.io", "owner")).token.raw, body: { role: "superuser" } })).status, 400);
});

test("last owner cannot be demoted or removed; a second owner can", async () => {
  const { call, owner, member } = setup();
  const { user, ws } = await owner("owner@x.io", "ws-a");
  const o1 = await member(ws.id, "owner@x.io", "owner");
  const base = `/v1/workspaces/${ws.id}/members`;
  const r = await call("PATCH", `${base}/${user.id}`, { token: o1.token.raw, body: { role: "admin" } });
  assert.equal(r.status, 409);
  assert.equal((await call("DELETE", `${base}/${user.id}`, { token: o1.token.raw })).status, 409);
  await member(ws.id, "second@x.io", "owner");
  assert.equal((await call("PATCH", `${base}/${user.id}`, { token: o1.token.raw, body: { role: "admin" } })).status, 200);
});

test("token scope widening is impossible", async () => {
  const { call, owner, member } = setup();
  const { ws } = await owner("owner@x.io", "ws-a");
  const ed = await member(ws.id, "ed@x.io", "editor");
  const url = `/v1/workspaces/${ws.id}/tokens`;
  // user-level: cannot grant scopes the role lacks
  for (const scope of ["member:manage", "audit:read", "deployment:create", "token:revoke_any", "workspace:delete"]) {
    const r = await call("POST", url, { token: ed.token.raw, body: { name: "x", scopes: [scope] } });
    assert.equal(r.status, 403, scope); assert.equal(j(r).error.code, "scope_exceeds_permissions");
  }
  assert.equal((await call("POST", url, { token: ed.token.raw, body: { name: "x", scopes: ["nonsense:perm"] } })).status, 400);
  assert.equal((await call("POST", url, { token: ed.token.raw, body: { name: "x", scopes: [] } })).status, 400);
  assert.equal((await call("POST", url, { token: ed.token.raw, body: { name: "x", scopes: ["*"] } })).status, 400);

  // token-level: a narrow token (read + token:create) cannot mint a wider one even though its creator could
  const narrow = await member(ws.id, "narrow@x.io", "editor", ["component:read", "token:create"]);
  const r = await call("POST", url, { token: narrow.token.raw, body: { name: "wide", scopes: ["component:write"] } });
  assert.equal(r.status, 403);
  const okR = await call("POST", url, { token: narrow.token.raw, body: { name: "same", scopes: ["component:read"], expiresInSeconds: 31536000 } });
  assert.equal(okR.status, 201);
  // child can never outlive its parent
  const parentExp = Date.parse("2026-01-01T00:00:00Z") + 86400_000;
  assert.ok(Date.parse(j(okR).expiresAt) <= parentExp);
  // and the child token really only carries the narrow scope
  const child = j(okR).token as string;
  assert.equal((await call("GET", "/v1/components", { token: child })).status, 200);
  assert.equal((await call("POST", "/v1/components", { token: child, body: { name: "c" } })).status, 403);
  // out-of-range / malformed lifetimes
  for (const bad of [1, -5, 1e12, "10", 1.5]) assert.equal((await call("POST", url, { token: ed.token.raw, body: { name: "x", scopes: ["component:read"], expiresInSeconds: bad } })).status, 400, String(bad));
});

test("token powers track the creator's current membership (demotion shrinks, removal kills)", async () => {
  const { call, owner, member, authRepo } = setup();
  const { ws } = await owner("owner@x.io", "ws-a");
  const ed = await member(ws.id, "ed@x.io", "editor");
  assert.equal((await call("POST", "/v1/components", { token: ed.token.raw, body: { name: "c1" } })).status, 201);
  await authRepo.setMember(ws.id, ed.user.id, "viewer");
  assert.equal((await call("POST", "/v1/components", { token: ed.token.raw, body: { name: "c2" } })).status, 403, "token scope still says write, role no longer does");
  assert.equal((await call("GET", "/v1/components", { token: ed.token.raw })).status, 200);
  await authRepo.removeMember(ws.id, ed.user.id);
  assert.equal((await call("GET", "/v1/components", { token: ed.token.raw })).status, 401);
});

test("workspace creation is for session users only; creator owns it", async () => {
  const { call, owner, member, login } = setup();
  const { ws } = await owner("o@x.io", "ws-a");
  const t = await member(ws.id, "ad@x.io", "admin");
  assert.equal((await call("POST", "/v1/workspaces", { token: t.token.raw, body: { slug: "ws-new", name: "n" } })).status, 403);
  const s = await login("ad@x.io");
  const r = await call("POST", "/v1/workspaces", { cookie: s.cookie, csrf: s.csrf, body: { slug: "ws-new", name: "n" } });
  assert.equal(r.status, 201);
  assert.equal((await call("POST", "/v1/workspaces", { cookie: s.cookie, csrf: s.csrf, body: { slug: "ws-new", name: "n" } })).status, 409);
  assert.equal((await call("POST", "/v1/workspaces", { cookie: s.cookie, csrf: s.csrf, body: { slug: "11111111-2222-3333-4444-555555555555", name: "n" } })).status, 400, "uuid-like slugs are refused");
  assert.equal((await call("POST", "/v1/workspaces", { cookie: s.cookie, csrf: s.csrf, body: { slug: "Bad Slug", name: "n" } })).status, 400);
  const mine = j(await call("GET", "/v1/workspaces", { cookie: s.cookie })).items;
  assert.deepEqual(mine.map((w: any) => [w.slug, w.role]).sort(), [["ws-a", "admin"], ["ws-new", "owner"]]);
});

// ---- cross-workspace isolation ---------------------------------------------------------------------
test("cross-workspace access is denied for users and tokens", async () => {
  const { call, owner, member, login } = setup();
  const A = await owner("a@x.io", "ws-a");
  const B = await owner("b@x.io", "ws-b");
  const aOwner = await member(A.ws.id, "a@x.io", "owner");
  const bOwner = await member(B.ws.id, "b@x.io", "owner");
  const bTok = (await call("POST", `/v1/workspaces/${B.ws.id}/tokens`, { token: bOwner.token.raw, body: { name: "k", scopes: ["component:read"] } }));
  assert.equal(bTok.status, 201);

  // path-scoped routes: B is invisible to A's owner (404, not 403), by id and by slug
  for (const ref of [B.ws.id, "ws-b"]) {
    for (const [m, p, body] of [["GET", "", undefined], ["PATCH", "", { name: "pwn" }], ["DELETE", "", undefined], ["GET", "/members", undefined], ["POST", "/members", { email: "a@x.io", role: "owner" }],
      ["GET", "/tokens", undefined], ["POST", "/tokens", { name: "t", scopes: ["component:read"] }], ["GET", "/audit", undefined]] as const) {
      const r = await call(m, `/v1/workspaces/${ref}${p}`, { token: aOwner.token.raw, body });
      assert.equal(r.status, 404, `${m} ${ref}${p} -> ${r.status}`);
    }
  }
  // revoking B's token through A's workspace path: 404 (token belongs elsewhere)
  assert.equal((await call("DELETE", `/v1/workspaces/${A.ws.id}/tokens/${j(bTok).id}`, { token: aOwner.token.raw })).status, 404);
  assert.equal((await call("DELETE", `/v1/workspaces/${B.ws.id}/tokens/${j(bTok).id}`, { token: aOwner.token.raw })).status, 404);
  // context routes: a token bound to A cannot be pointed at B
  assert.equal((await call("GET", "/v1/components", { token: aOwner.token.raw, ws: B.ws.id })).status, 403);
  assert.equal((await call("GET", "/v1/components", { token: aOwner.token.raw, ws: "ws-b" })).status, 403);
  assert.equal((await call("POST", "/v1/components", { token: aOwner.token.raw, ws: "ws-b", body: { name: "x" } })).status, 403);
  assert.equal((await call("GET", "/v1/components", { token: aOwner.token.raw, ws: A.ws.id })).status, 200);
  // sessions: not a member of B -> header is refused
  const s = await login("a@x.io");
  assert.equal((await call("GET", "/v1/components", { cookie: s.cookie, ws: B.ws.id })).status, 403);
  // a user with admin in A and viewer in B has B's (lower) rights in B, not A's
  await member(B.ws.id, "a@x.io", "viewer");
  const s2 = await login("a@x.io");
  assert.equal((await call("POST", "/v1/components", { cookie: s2.cookie, csrf: s2.csrf, ws: "ws-b", body: { name: "x" } })).status, 403);
  assert.equal((await call("GET", `/v1/workspaces/ws-b/audit`, { cookie: s2.cookie })).status, 403);
  // B's audit log stays B's
  const aud = j(await call("GET", `/v1/workspaces/${A.ws.id}/audit`, { token: aOwner.token.raw }));
  assert.ok(aud.items.every((e: any) => e.workspaceId === A.ws.id));
  // B's token cannot be used on A either
  const bRaw = j(bTok).token as string;
  assert.equal((await call("GET", "/v1/components", { token: bRaw, ws: A.ws.id })).status, 403);
  assert.equal((await call("GET", `/v1/workspaces/${A.ws.id}`, { token: bRaw })).status, 404);
});

test("runtime role holds no connection, secret or test_run permission; connection:read is widely held", () => {
  for (const p of ROLE_PERMISSIONS.runtime) assert.ok(!/^(connection|secret|test_run):/.test(p), p);
  for (const r of ["owner", "admin", "editor", "viewer", "deployer"] as Role[]) assert.ok(ROLE_PERMISSIONS[r].includes("connection:read"), r);
  for (const r of ["viewer", "deployer"] as Role[]) for (const p of ["connection:write", "connection:delete", "secret:write", "secret:delete", "test_run:execute"] as const) assert.ok(!ROLE_PERMISSIONS[r].includes(p), `${r} ${p}`);
});
