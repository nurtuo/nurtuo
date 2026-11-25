// SPDX-License-Identifier: AGPL-3.0-only
// Authentication: login, lockout, rate limit, sessions, API tokens, JWT tampering, audit.
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { test } from "node:test";
import { createHandler } from "../api.js";
import { MemoryRepository } from "../memory-repository.js";
import { b64u } from "./crypto.js";
import { PASSWORD, j, setup } from "./test-util.js";

test("auth disabled is explicit and keeps legacy behaviour", async () => {
  const h = createHandler(new MemoryRepository(), { auth: "disabled" });
  assert.equal((await h({ method: "GET", path: "/v1/components" })).status, 200);
  // Workspace creation is not reachable without the auth hook (instance config is not parsed then).
  assert.equal((await h({ method: "POST", path: "/v1/workspaces", body: { slug: "mine", name: "m" } })).status, 405);
  const h2 = createHandler(new MemoryRepository()); // default (dev/test) is the same
  assert.equal((await h2({ method: "GET", path: "/v1/components" })).status, 200);
});

test("login: success sets a hardened cookie; failures are generic", async () => {
  const { call, owner, login } = setup();
  await owner("u@x.io", "ws-a");
  const ok = await login("U@X.io"); // email is case-insensitive
  const cookieHdr = String((ok.r.headers!["set-cookie"] as string[])[0]);
  assert.match(cookieHdr, /^__Host-nurtuo_session=/);
  for (const a of ["HttpOnly", "Secure", "SameSite=Strict", "Path=/"]) assert.ok(cookieHdr.includes(a), a);
  assert.ok(!cookieHdr.includes("Domain"));
  assert.equal(JSON.stringify(j(ok.r)).includes("passwordHash"), false);
  assert.equal(JSON.stringify(j(ok.r)).includes("scrypt"), false);
  const wrongPw = await call("POST", "/v1/auth/login", { body: { email: "u@x.io", password: "wrong-wrong-wrong" } });
  const noUser = await call("POST", "/v1/auth/login", { body: { email: "ghost@x.io", password: "wrong-wrong-wrong" } });
  assert.equal(wrongPw.status, 401); assert.deepEqual(wrongPw.body, noUser.body, "no user enumeration");
  assert.equal((await call("POST", "/v1/auth/login", { body: { email: "u@x.io" } })).status, 401);
  assert.equal((await call("POST", "/v1/auth/login", { body: { email: ["u@x.io"], password: { $ne: 1 } } })).status, 401);
  assert.equal((await call("POST", "/v1/auth/login", { body: { email: "u@x.io", password: "x".repeat(100_000) } })).status, 401);
  // stale/garbage cookie must not block logging in
  assert.equal((await call("POST", "/v1/auth/login", { cookie: "garbage", body: { email: "u@x.io", password: PASSWORD } })).status, 200);
});

test("lockout after repeated failures (existing and unknown accounts), then recovery", async () => {
  const { call, owner, clock } = setup();
  await owner("u@x.io", "ws-a");
  const bad = (email: string) => call("POST", "/v1/auth/login", { body: { email, password: "wrong-wrong-wrong" } });
  for (let i = 0; i < 5; i++) assert.equal((await bad("u@x.io")).status, 401);
  const locked = await call("POST", "/v1/auth/login", { body: { email: "u@x.io", password: PASSWORD } });
  assert.equal(locked.status, 429, "correct password is refused while locked");
  assert.ok(Number(locked.headers!["retry-after"]) > 0);
  for (let i = 0; i < 5; i++) assert.equal((await bad("ghost@x.io")).status, 401);
  const ghost = await bad("ghost@x.io");
  assert.equal(ghost.status, 429, "unknown emails lock out identically");
  assert.equal(j(ghost).error.code, j(locked).error.code);
  clock.t += 16 * 60_000;
  assert.equal((await call("POST", "/v1/auth/login", { body: { email: "u@x.io", password: PASSWORD } })).status, 200);
});

test("successful login resets the failure counter", async () => {
  const { call, owner } = setup();
  await owner("u@x.io", "ws-a");
  const bad = () => call("POST", "/v1/auth/login", { body: { email: "u@x.io", password: "wrong-wrong-wrong" } });
  for (let k = 0; k < 3; k++) { for (let i = 0; i < 4; i++) await bad(); assert.equal((await call("POST", "/v1/auth/login", { body: { email: "u@x.io", password: PASSWORD } })).status, 200); }
});

test("per-IP rate limit on login", async () => {
  const { call } = setup({ service: { loginsPerMinutePerIp: 3 } });
  const t = (ip: string) => call("POST", "/v1/auth/login", { ip, body: { email: "a@x.io", password: "wrong-wrong-wrong" } });
  for (let i = 0; i < 3; i++) assert.equal((await t("1.1.1.1")).status, 401);
  const r = await t("1.1.1.1");
  assert.equal(r.status, 429); assert.ok(Number(r.headers!["retry-after"]) >= 1);
  assert.equal((await t("2.2.2.2")).status, 401, "other IPs unaffected");
});

test("sessions: expiry, revocation, disabled users, tampered or foreign JWTs", async () => {
  const { call, owner, login, clock, authRepo, service } = setup();
  const { user } = await owner("u@x.io", "ws-a");
  const s = await login("u@x.io");
  assert.equal((await call("GET", "/v1/auth/me", { cookie: s.cookie })).status, 200);
  // Bearer use of the session JWT works too and does not need CSRF
  assert.equal((await call("POST", "/v1/components", { token: s.cookie, body: { name: "viabearer" } })).status, 201);
  // tampering
  const [h, p, sig] = s.cookie.split(".") as [string, string, string];
  const claims = JSON.parse(Buffer.from(p, "base64url").toString());
  const forged = b64u(JSON.stringify({ ...claims, sub: "someone-else" }));
  assert.equal((await call("GET", "/v1/auth/me", { cookie: `${h}.${forged}.${sig}` })).status, 401);
  const none = `${b64u(JSON.stringify({ alg: "none", typ: "JWT" }))}.${p}.`;
  assert.equal((await call("GET", "/v1/auth/me", { cookie: none })).status, 401);
  // valid signature from a different secret
  const other = setup();
  const o2 = await other.owner("u@x.io", "ws-a"); void o2;
  assert.equal((await call("GET", "/v1/auth/me", { cookie: (await other.login("u@x.io")).cookie })).status, 401);
  // correctly signed JWT for a session id that does not exist
  const body = b64u(JSON.stringify({ ...claims, sid: "no-such-session" }));
  const mac = b64u(createHmac("sha256", Buffer.alloc(32)).update(`${h}.${body}`).digest());
  assert.equal((await call("GET", "/v1/auth/me", { cookie: `${h}.${body}.${mac}` })).status, 401);
  // disabling the user kills existing sessions immediately
  await authRepo.setUserStatus(user.id, "disabled");
  assert.equal((await call("GET", "/v1/auth/me", { cookie: s.cookie })).status, 401);
  assert.equal((await call("POST", "/v1/auth/login", { body: { email: "u@x.io", password: PASSWORD } })).status, 401);
  await authRepo.setUserStatus(user.id, "active");
  assert.equal((await call("GET", "/v1/auth/me", { cookie: s.cookie })).status, 200);
  // expiry
  clock.t += (service.sessionTtlSec + 1) * 1000;
  assert.equal((await call("GET", "/v1/auth/me", { cookie: s.cookie })).status, 401);
});

test("api tokens: bearer auth, hashed at rest, expiry, revocation, last-used", async () => {
  const { call, owner, member, authRepo, clock, mint } = setup();
  const { ws } = await owner("o@x.io", "ws-a");
  const adm = await member(ws.id, "ad@x.io", "admin");
  const c = await call("POST", `/v1/workspaces/${ws.id}/tokens`, { token: adm.token.raw, body: { name: "ci", scopes: ["component:read", "component:write"], expiresInSeconds: 3600 } });
  assert.equal(c.status, 201);
  const raw = j(c).token as string;
  assert.match(raw, /^nrt_[0-9a-f]{12}_[A-Za-z0-9_-]{43}$/);
  const rec = (await authRepo.getToken(j(c).id))!;
  // The secret is base64url and may itself contain "_", so take everything after "nrt_<prefix>_".
  assert.ok(!JSON.stringify(rec).includes(raw.slice("nrt_".length + 12 + 1)), "secret is not stored");
  assert.match(rec.hash, /^[0-9a-f]{64}$/);
  assert.equal(j(c).hash, undefined);
  assert.equal(rec.lastUsedAt, null);
  assert.equal((await call("POST", "/v1/components", { token: raw, body: { name: "n1" } })).status, 201);
  assert.ok((await authRepo.getToken(rec.id))!.lastUsedAt, "last_used_at recorded");
  // listing never leaks hashes or secrets
  const list = JSON.stringify(j(await call("GET", `/v1/workspaces/${ws.id}/tokens`, { token: adm.token.raw })));
  assert.ok(!list.includes(rec.hash) && !list.includes(raw));
  // wrong secret / wrong prefix / malformed: identical 401
  const wrongSecret = `${raw.slice(0, -3)}AAA`;
  const unknownPrefix = `nrt_${"0".repeat(12)}_${raw.split("_")[2]}`;
  const outs = await Promise.all([wrongSecret, unknownPrefix, "nrt_garbage", "Bearer x", "abc"].map((t) => call("GET", "/v1/components", { token: t, ws: ws.id })));
  for (const r of outs) assert.equal(r.status, 401);
  assert.deepEqual(outs[0]!.body, outs[1]!.body, "unknown prefix and wrong secret are indistinguishable");
  assert.ok(String(outs[0]!.headers!["www-authenticate"]).startsWith("Bearer"));
  // malformed Authorization header
  assert.equal((await call("GET", "/v1/components", { headers: { authorization: "Basic dTpw" } })).status, 401);
  // expiry
  clock.t += 3601_000;
  assert.equal((await call("GET", "/v1/components", { token: raw })).status, 401);
  // revocation (admin revokes another user's token; editor cannot)
  const t2 = await mint(ws.id, (await authRepo.getUserByEmail("ad@x.io"))!.id, ["component:read"], 86400_000 * 2);
  const ed = await member(ws.id, "ed@x.io", "editor");
  assert.equal((await call("DELETE", `/v1/workspaces/${ws.id}/tokens/${t2.id}`, { token: ed.token.raw })).status, 403);
  assert.equal((await call("GET", "/v1/components", { token: t2.raw })).status, 200);
  assert.equal((await call("DELETE", `/v1/workspaces/${ws.id}/tokens/${t2.id}`, { token: adm.token.raw })).status, 204);
  assert.equal((await call("GET", "/v1/components", { token: t2.raw })).status, 401);
  // a creator can revoke their own token
  assert.equal((await call("DELETE", `/v1/workspaces/${ws.id}/tokens/${ed.token.id}`, { token: ed.token.raw })).status, 204);
  assert.equal((await call("GET", "/v1/components", { token: ed.token.raw })).status, 401);
});

test("removing a member revokes their tokens in the workspace", async () => {
  const { call, owner, member, authRepo } = setup();
  const { ws } = await owner("o@x.io", "ws-a");
  const ed = await member(ws.id, "ed@x.io", "editor");
  const own = await member(ws.id, "o@x.io", "owner");
  assert.equal((await call("DELETE", `/v1/workspaces/${ws.id}/members/${ed.user.id}`, { token: own.token.raw })).status, 204);
  assert.ok((await authRepo.getToken(ed.token.id))!.revokedAt);
});

test("audit: every mutating request is logged, reads are not, secrets never appear", async () => {
  const { call, owner, member, authRepo } = setup();
  const { ws } = await owner("o@x.io", "ws-a");
  const ed = await member(ws.id, "ed@x.io", "editor");
  const vw = await member(ws.id, "vw@x.io", "viewer");
  const own = await member(ws.id, "o@x.io", "owner");

  const created = await call("POST", "/v1/components", { token: ed.token.raw, body: { name: "orders" }, headers: { "x-request-id": "req-12345678" } });
  assert.equal(created.headers!["x-request-id"], "req-12345678");
  const id = j(created).id as string;
  await call("PATCH", `/v1/components/${id}`, { token: ed.token.raw, body: { description: "d" } });
  await call("POST", "/v1/components", { token: ed.token.raw, body: { name: "orders" } }); // 409 conflict still audited
  await call("POST", "/v1/components", { token: vw.token.raw, body: { name: "denied" } }); // 403 audited
  await call("GET", "/v1/components", { token: ed.token.raw }); // reads are not audited
  await call("POST", "/v1/components", { token: "nrt_" + "0".repeat(12) + "_" + "A".repeat(43), body: { name: "x" } }); // 401 not audited
  const tok = await call("POST", `/v1/workspaces/${ws.id}/tokens`, { token: own.token.raw, body: { name: "k", scopes: ["component:read"] } });

  const page = (await authRepo.queryAudit({ workspaceId: ws.id, limit: 100 })).items.reverse();
  const acts = page.map((e) => `${e.action}:${e.status}`);
  assert.deepEqual(acts, ["component.create:201", "component.update:200", "component.create:409", "component.create:403", "token.create:201"]);
  const [c, u, , denied, t] = page as [any, any, any, any, any];
  assert.equal(c.actorType, "token"); assert.equal(c.actorId, ed.token.id); assert.equal(c.requestId, "req-12345678");
  assert.equal(c.resourceId, id); assert.equal(c.beforeHash, null); assert.match(c.afterHash, /^sha256:[0-9a-f]{64}$/);
  assert.equal(c.ip, "10.0.0.1"); assert.equal(c.metadata.userId, ed.user.id);
  assert.match(u.beforeHash, /^sha256:/); assert.notEqual(u.beforeHash, u.afterHash, "before/after differ across the PATCH");
  assert.equal(denied.metadata.denied, "forbidden");
  assert.ok(!JSON.stringify(page).includes(j(tok).token), "plaintext token never reaches the audit log");
  assert.equal(t.resourceId, j(tok).id);
  // entries are immutable in memory too
  assert.throws(() => { (c as any).action = "x"; });
});

test("audit endpoint: filters and cursor pagination", async () => {
  const { call, owner, member } = setup();
  const { ws } = await owner("o@x.io", "ws-a");
  const ad = await member(ws.id, "ad@x.io", "admin");
  const ed = await member(ws.id, "ed@x.io", "editor");
  for (let i = 0; i < 5; i++) await call("POST", "/v1/components", { token: ed.token.raw, body: { name: `c${i}` } });
  await call("POST", "/v1/runtimes", { token: ad.token.raw, ws: ws.id, body: { id: "rt", name: "n", environment: "dev" } });
  await call("POST", "/v1/components", { token: ed.token.raw, body: { name: "BAD NAME" } });
  const url = `/v1/workspaces/${ws.id}/audit`;
  const q = (query: Record<string, string>) => call("GET", url, { token: ad.token.raw, query });

  const all = j(await q({}));
  assert.equal(all.items.length, 7);
  assert.deepEqual(all.items.map((e: any) => e.seq), [...all.items.map((e: any) => e.seq)].sort((a: number, b: number) => b - a), "newest first");
  // pagination walks every row exactly once
  const seen: number[] = [];
  let cursor: string | undefined;
  for (let guard = 0; guard < 10; guard++) {
    const pg = j(await q(cursor ? { limit: "3", cursor } : { limit: "3" }));
    seen.push(...pg.items.map((e: any) => e.seq));
    if (!pg.nextCursor) break;
    cursor = pg.nextCursor;
  }
  assert.deepEqual(seen, all.items.map((e: any) => e.seq));
  // filters
  assert.equal(j(await q({ action: "component.create", status: "201" })).items.length, 5);
  assert.equal(j(await q({ action: "component.create", status: "400" })).items.length, 1);
  assert.equal(j(await q({ resource: "runtime" })).items.length, 1);
  assert.equal(j(await q({ actor: ed.token.id })).items.length, 6);
  assert.equal(j(await q({ from: "2999-01-01T00:00:00Z" })).items.length, 0);
  assert.equal(j(await q({ to: "2999-01-01T00:00:00Z" })).items.length, 7);
  // validation
  for (const bad of <Record<string, string>[]>[{ limit: "0" }, { limit: "201" }, { limit: "x" }, { cursor: "abc" }, { from: "yesterday" }, { status: "ok" }]) assert.equal((await q(bad)).status, 400, JSON.stringify(bad));
  // role gate
  assert.equal((await call("GET", url, { token: ed.token.raw })).status, 403);
});

test("security headers on responses", async () => {
  const { call } = setup();
  const r = await call("GET", "/v1/components");
  assert.equal(r.headers!["cache-control"], "no-store");
  assert.ok(r.headers!["x-request-id"]);
  // hostile request ids are replaced, not echoed (header injection)
  const r2 = await call("GET", "/v1/components", { headers: { "x-request-id": "a\r\nSet-Cookie: x=1" } });
  assert.ok(!String(r2.headers!["x-request-id"]).includes("Set-Cookie"));
});

test("SAML and SCIM are documented stubs returning 501", async () => {
  const { call } = setup();
  for (const [m, p] of [["GET", "/v1/auth/saml/login"], ["POST", "/v1/auth/saml/acs"], ["GET", "/v1/auth/saml/metadata"], ["GET", "/v1/scim/v2/Users"], ["POST", "/v1/scim/v2/Users"], ["DELETE", "/v1/scim/v2/Users/abc"], ["GET", "/v1/scim/v2/Groups"]] as const) {
    const r = await call(m, p, { body: {} });
    assert.equal(r.status, 501, `${m} ${p}`);
  }
  assert.equal(j(await call("GET", "/v1/auth/saml/login")).error.code, "not_implemented");
});

test("trustProxy only honoured when enabled", async () => {
  const mk = async (trust: boolean) => {
    const { call, owner, member, authRepo } = setup({ hook: { trustProxy: trust } });
    const { ws } = await owner("o@x.io", "ws-a");
    const ed = await member(ws.id, "ed@x.io", "editor");
    await call("POST", "/v1/components", { token: ed.token.raw, body: { name: "a" }, headers: { "x-forwarded-for": "203.0.113.9, 10.0.0.2" } });
    return (await authRepo.queryAudit({ workspaceId: ws.id, limit: 1 })).items[0]!.ip;
  };
  assert.equal(await mk(false), "10.0.0.1");
  assert.equal(await mk(true), "203.0.113.9");
});

test("audit failure fails the request closed", async () => {
  const { call, owner, member, authRepo } = setup();
  const { ws } = await owner("o@x.io", "ws-a");
  const ed = await member(ws.id, "ed@x.io", "editor");
  authRepo.appendAudit = async () => { throw new Error("db down"); };
  const r = await call("POST", "/v1/components", { token: ed.token.raw, body: { name: "a" } });
  assert.equal(r.status, 500); assert.equal(j(r).error.code, "audit_unavailable");
});

test("auth extension receives the resolved workspace and cancellation signal", async () => {
  const signal = new AbortController().signal;
  const { owner, login, handle } = setup({ hook: { extraEndpoints: {
    async "component.list"(ctx) {
      assert.equal(ctx.signal, signal);
      return { status: 200, body: { workspaceId: ctx.workspace?.id } };
    },
  } } });
  const { ws } = await owner("extension@x.io", "extension");
  const { cookie } = await login("extension@x.io");
  const response = await handle({ method: "GET", path: "/v1/components", signal,
    headers: { cookie: `__Host-nurtuo_session=${cookie}`, "x-nurtuo-workspace": ws.id } });
  assert.deepEqual(response.body, { workspaceId: ws.id });
});

test("workspace deletion is serialized and cleanup follows successful deletion only", async () => {
  const events: string[] = [];
  const { owner, login, call, authRepo } = setup({ hook: {
    extraEndpoints: { async "workspace.delete"() { throw new Error("built-in endpoint overridden"); } },
    async serializeWorkspaceDelete(id, remove) {
      assert.ok(await authRepo.getWorkspace(id));
      events.push("serialized");
      const result = await remove();
      assert.equal(await authRepo.getWorkspace(id), undefined);
      events.push("deleted");
      return result;
    },
    async onWorkspaceDeleted(id) {
      assert.equal(await authRepo.getWorkspace(id), undefined);
      events.push("cleaned");
    },
  } });
  const { ws } = await owner("cleanup@x.io", "cleanup");
  const { cookie, csrf } = await login("cleanup@x.io");
  const path = `/v1/workspaces/${ws.id}`;
  assert.equal((await call("DELETE", path, { cookie })).status, 403);
  assert.deepEqual(events, []);
  assert.equal((await call("DELETE", path, { cookie, csrf })).status, 204);
  assert.deepEqual(events, ["serialized", "deleted", "cleaned"]);
});
