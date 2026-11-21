// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { createHash, createSign, generateKeyPairSync, randomBytes, type KeyObject } from "node:crypto";
import { test } from "node:test";
import { b64u } from "./crypto.js";
import { createAuthHook } from "./middleware.js";
import { OidcClient, type OidcFetch } from "./oidc.js";
import { j, setup } from "./test-util.js";

const ISS = "https://idp.example.test";
const CLIENT = "nurtuo-client";

/** A fake IdP: discovery, JWKS and token endpoints served from an injected fetch. */
class FakeIdp {
  key = generateKeyPairSync("rsa", { modulusLength: 2048 });
  kid = "k1";
  codes = new Map<string, { challenge: string; nonce: string; claims: Record<string, unknown> }>();
  tokenRequests: URLSearchParams[] = [];
  /** per-test tweaks */
  mutate: (c: Record<string, unknown>) => Record<string, unknown> = (c) => c;
  alg = "RS256";
  signWith: KeyObject = this.key.privateKey;
  fetch: OidcFetch = async (url, init) => {
    const u = new URL(url);
    const ok = (b: unknown) => ({ ok: true, status: 200, json: async () => b });
    if (u.pathname === "/.well-known/openid-configuration") return ok({ issuer: ISS, authorization_endpoint: `${ISS}/authorize`, token_endpoint: `${ISS}/token`, jwks_uri: `${ISS}/jwks` });
    if (u.pathname === "/jwks") return ok({ keys: [{ ...this.key.publicKey.export({ format: "jwk" }), kid: this.kid, use: "sig", alg: "RS256" }] });
    if (u.pathname === "/token") {
      const p = new URLSearchParams(init!.body!);
      this.tokenRequests.push(p);
      const rec = this.codes.get(p.get("code")!);
      if (!rec || p.get("client_id") !== CLIENT) return { ok: false, status: 400, json: async () => ({ error: "invalid_grant" }) };
      // PKCE: the verifier must hash to the challenge sent at /authorize
      if (createHash("sha256").update(p.get("code_verifier") ?? "").digest("base64url") !== rec.challenge) return { ok: false, status: 400, json: async () => ({ error: "invalid_grant" }) };
      this.codes.delete(p.get("code")!);
      const now = Math.floor(Date.now() / 1000);
      const claims = this.mutate({ iss: ISS, aud: CLIENT, iat: now, exp: now + 300, nonce: rec.nonce, ...rec.claims });
      const h = b64u(JSON.stringify({ alg: this.alg, typ: "JWT", kid: this.kid }));
      const body = b64u(JSON.stringify(claims));
      const sig = createSign("RSA-SHA256").update(`${h}.${body}`).sign(this.signWith);
      return ok({ id_token: `${h}.${body}.${b64u(sig)}`, access_token: "at", token_type: "Bearer" });
    }
    return { ok: false, status: 404, json: async () => ({}) };
  };
  /** Plays the browser: follows the authorization redirect and returns the callback query. */
  authorize(location: string, claims: Record<string, unknown>): { code: string; state: string } {
    const q = new URL(location).searchParams;
    assert.equal(q.get("code_challenge_method"), "S256");
    assert.equal(q.get("response_type"), "code");
    assert.equal(q.get("client_id"), CLIENT);
    const code = randomBytes(8).toString("hex");
    this.codes.set(code, { challenge: q.get("code_challenge")!, nonce: q.get("nonce")!, claims });
    return { code, state: q.get("state")! };
  }
}

function world(opts: { jit?: boolean; link?: boolean } = {}) {
  const idp = new FakeIdp();
  const provider = new OidcClient({ issuer: ISS, clientId: CLIENT, clientSecret: "shh", redirectUri: "https://cp.test/v1/auth/oidc/callback", fetch: idp.fetch });
  const w = setup({ hook: { oidc: { provider, issuer: ISS, jit: opts.jit ? { workspace: "ws-a", role: "viewer" } : undefined, linkByVerifiedEmail: opts.link } } });
  return { idp, ...w };
}
const oidcCookie = (r: { headers?: Record<string, string | string[]> }) => /__Host-nurtuo_oidc=([^;]+)/.exec(String(r.headers!["set-cookie"]))![1]!;
async function begin(w: ReturnType<typeof world>) {
  const r = await w.call("GET", "/v1/auth/oidc/login");
  assert.equal(r.status, 302);
  return { r, location: String(r.headers!.location), cookie: oidcCookie(r) };
}
const callback = (w: ReturnType<typeof world>, q: Record<string, string>, cookie?: string) =>
  w.call("GET", "/v1/auth/oidc/callback", { query: q, headers: cookie ? { cookie: `__Host-nurtuo_oidc=${cookie}` } : {} });

test("oidc: authorization code + PKCE, JIT provisioning, session issued", async () => {
  const w = world({ jit: true });
  await w.owner("o@x.io", "ws-a");
  const b = await begin(w);
  assert.match(String(b.r.headers!["set-cookie"]), /HttpOnly; Secure; SameSite=Lax/);
  const cb = w.idp.authorize(b.location, { sub: "idp-user-1", email: "New@Corp.io", email_verified: true, name: "New U" });
  const r = await callback(w, cb, b.cookie);
  assert.equal(r.status, 200, JSON.stringify(r.body));
  assert.equal(j(r).user.email, "new@corp.io");
  assert.equal(w.idp.tokenRequests[0]!.get("client_secret"), "shh");
  const cookies = r.headers!["set-cookie"] as string[];
  const session = /__Host-nurtuo_session=([^;]+)/.exec(cookies.join("\n"))![1]!;
  const me = j(await w.call("GET", "/v1/auth/me", { cookie: session }));
  assert.deepEqual(me.workspaces.map((x: any) => x.role), ["viewer"], "JIT role is the configured least-privilege role");
  // second login maps to the same user by (issuer, sub)
  const b2 = await begin(w);
  const r2 = await callback(w, w.idp.authorize(b2.location, { sub: "idp-user-1", email: "changed@corp.io", email_verified: true }), b2.cookie);
  assert.equal(j(r2).user.id, j(r).user.id);
});

test("oidc: no JIT and no link -> sso_not_provisioned; link by verified email is opt-in", async () => {
  const w = world();
  await w.owner("victim@x.io", "ws-a");
  const b = await begin(w);
  const r = await callback(w, w.idp.authorize(b.location, { sub: "s1", email: "victim@x.io", email_verified: true }), b.cookie);
  assert.equal(r.status, 403); assert.equal(j(r).error.code, "sso_not_provisioned");
  const w2 = world({ link: true });
  await w2.owner("victim@x.io", "ws-a");
  const b2 = await begin(w2);
  assert.equal((await callback(w2, w2.idp.authorize(b2.location, { sub: "s1", email: "victim@x.io", email_verified: true }), b2.cookie)).status, 200);
  // unverified email is never used for linking, even when linking is on
  const b3 = await begin(w2);
  assert.equal((await callback(w2, w2.idp.authorize(b3.location, { sub: "s2", email: "victim@x.io", email_verified: false }), b3.cookie)).status, 403);
});

test("oidc: attack cases are rejected", async () => {
  const run = async (tweak: (idp: FakeIdp) => void, claims: Record<string, unknown> = {}, mutateCb?: (q: { code: string; state: string }, cookie: string) => { q: Record<string, string>; cookie?: string }) => {
    const w = world({ jit: true });
    await w.owner("o@x.io", "ws-a");
    tweak(w.idp);
    const b = await begin(w);
    const q = w.idp.authorize(b.location, { sub: "u1", email: "u@corp.io", email_verified: true, ...claims });
    const m = mutateCb ? mutateCb(q, b.cookie) : { q, cookie: b.cookie };
    return callback(w, m.q, m.cookie);
  };
  const ok = await run(() => {});
  assert.equal(ok.status, 200);
  assert.equal((await run((i) => (i.mutate = (c) => ({ ...c, nonce: "other" })))).status, 401, "nonce mismatch");
  assert.equal((await run((i) => (i.mutate = (c) => ({ ...c, aud: "someone-else" })))).status, 401, "audience");
  assert.equal((await run((i) => (i.mutate = (c) => ({ ...c, iss: "https://evil.test" })))).status, 401, "issuer");
  assert.equal((await run((i) => (i.mutate = (c) => ({ ...c, exp: Math.floor(Date.now() / 1000) - 3600 })))).status, 401, "expired");
  assert.equal((await run((i) => (i.alg = "none"))).status, 401, "alg none");
  assert.equal((await run((i) => (i.alg = "HS256"))).status, 401, "alg confusion");
  assert.equal((await run((i) => (i.signWith = generateKeyPairSync("rsa", { modulusLength: 2048 }).privateKey))).status, 401, "signed by an unknown key");
  assert.equal((await run(() => {}, {}, (q, c) => ({ q: { ...q, state: "forged" }, cookie: c }))).status, 401, "state mismatch");
  assert.equal((await run(() => {}, {}, (q) => ({ q, cookie: undefined }))).status, 401, "no state cookie (login CSRF)");
  assert.equal((await run(() => {}, {}, (q, c) => ({ q, cookie: c.slice(0, -2) + "xx" }))).status, 401, "tampered state cookie");
  assert.equal((await run(() => {}, {}, (q, c) => ({ q: { ...q, code: "stolen" }, cookie: c }))).status, 401, "unknown code");
});

test("oidc: authorization code cannot be replayed and PKCE verifier is bound to the browser", async () => {
  const w = world({ jit: true });
  await w.owner("o@x.io", "ws-a");
  const b = await begin(w);
  const cb = w.idp.authorize(b.location, { sub: "u1", email: "u@corp.io", email_verified: true });
  assert.equal((await callback(w, cb, b.cookie)).status, 200);
  assert.equal((await callback(w, cb, b.cookie)).status, 401, "replayed code");
  // attacker starts their own flow and swaps in the victim's code: verifier does not match the challenge
  const victim = await begin(w);
  const attacker = await begin(w);
  const victimCb = w.idp.authorize(victim.location, { sub: "u2", email: "v@corp.io", email_verified: true });
  assert.equal((await callback(w, { code: victimCb.code, state: attacker.location ? new URL(attacker.location).searchParams.get("state")! : "" }, attacker.cookie)).status, 401);
});

test("oidc: not configured -> 404; issuer must be https; JIT cannot grant owner/admin", async () => {
  const { call, service } = setup();
  for (const role of ["owner", "admin"] as const) assert.throws(() => createAuthHook({ service, oidc: { provider: {} as any, issuer: ISS, jit: { workspace: "w", role } } }));
  assert.equal((await call("GET", "/v1/auth/oidc/login")).status, 404);
  assert.throws(() => new OidcClient({ issuer: "http://idp.example.test", clientId: "c", redirectUri: "x" }));
});
