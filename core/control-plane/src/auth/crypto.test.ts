// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { createHmac, randomBytes } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { ALL_PERMISSIONS, ROLES, ROLE_DESCRIPTIONS, ROLE_PERMISSIONS, canGrant, permissionsOf } from "./permissions.js";
import { b64u, generateApiToken, hashPassword, parseApiToken, safeEqual, sha256hex, signJwt, verifyJwt, verifyPassword } from "./crypto.js";

const fast = { N: 1024, r: 8, p: 1 };

test("scrypt: verify, wrong password, salting, tampered params", async () => {
  const h = await hashPassword("s3cret-passphrase!", fast);
  assert.match(h, /^scrypt\$1024\$8\$1\$/);
  assert.equal(await verifyPassword("s3cret-passphrase!", h), true);
  assert.equal(await verifyPassword("s3cret-passphrase?", h), false);
  assert.notEqual(h, await hashPassword("s3cret-passphrase!", fast));
  // absurd cost in a tampered row is refused instead of DoS-ing the server
  assert.equal(await verifyPassword("x", h.replace("$1024$", "$1073741824$")), false);
  assert.equal(await verifyPassword("x", "plaintext"), false);
});

test("jwt: HS256 only, signature, expiry, iss/aud", () => {
  const secret = randomBytes(32);
  const now = 1_000_000;
  const good = signJwt({ iss: "i", aud: "a", sub: "u", exp: now + 60 }, secret);
  const v = (t: string, s = secret) => verifyJwt(t, s, { iss: "i", aud: "a", now });
  assert.equal(v(good)?.sub, "u");
  assert.equal(v(good, randomBytes(32)), null);
  assert.equal(verifyJwt(good, secret, { iss: "i", aud: "other", now }), null);
  assert.equal(verifyJwt(good, secret, { iss: "i", aud: "a", now: now + 61 }), null);
  const [h, p, s] = good.split(".") as [string, string, string];
  const tamperedBody = b64u(JSON.stringify({ iss: "i", aud: "a", sub: "admin", exp: now + 60 }));
  assert.equal(v(`${h}.${tamperedBody}.${s}`), null);
  // alg=none and alg=HS512 header swaps are rejected even with a (re)computed valid MAC
  const none = `${b64u(JSON.stringify({ alg: "none", typ: "JWT" }))}.${p}.`;
  assert.equal(v(none), null);
  const hdr512 = b64u(JSON.stringify({ alg: "HS512", typ: "JWT" }));
  assert.equal(v(`${hdr512}.${p}.${b64u(createHmac("sha256", secret).update(`${hdr512}.${p}`).digest())}`), null);
  assert.equal(v("not.a.jwt"), null);
  assert.equal(v(""), null);
});

test("api token format and hashing", () => {
  const g = generateApiToken();
  const p = parseApiToken(g.token)!;
  assert.equal(p.prefix, g.prefix);
  assert.equal(sha256hex(p.secret), g.hash);
  assert.ok(!g.token.includes(g.hash));
  assert.equal(parseApiToken("nrt_zz_short"), null);
  assert.equal(parseApiToken(`${g.token}x`), null);
});

test("safeEqual", () => {
  assert.equal(safeEqual("abc", "abc"), true);
  assert.equal(safeEqual("abc", "abd"), false);
  assert.equal(safeEqual("abc", "abcd"), false);
  assert.equal(safeEqual("", ""), true);
});

test("secrets are never compared with ===, ==, or Buffer.equals in auth code", () => {
  const dir = fileURLToPath(new URL("../../src/auth/", import.meta.url));
  const bad = /(\.hash|csrf|secret|\bstate|nonce|clientSecret|signature)\s*(===|!==)\s*[^=]/;
  for (const f of readdirSync(dir).filter((f) => f.endsWith(".ts") && !f.endsWith(".test.ts") && f !== "types.ts")) {
    const src = readFileSync(dir + f, "utf8").split("\n");
    src.forEach((line, i) => { if (!line.trim().startsWith("//") && bad.test(line) && !/typeof|undefined|null|"/.test(line)) assert.fail(`${f}:${i + 1} non-constant-time comparison: ${line.trim()}`); });
  }
});

test("permissions: matrix invariants", () => {
  for (const r of ROLES) for (const p of ROLE_PERMISSIONS[r]) assert.ok(ALL_PERMISSIONS.includes(p), `${r} has unknown ${p}`);
  assert.deepEqual([...permissionsOf("owner")].sort(), [...ALL_PERMISSIONS].sort());
  // the strict ordering that makes escalation checks meaningful
  assert.equal(canGrant(permissionsOf("admin"), "owner"), false);
  assert.equal(canGrant(permissionsOf("admin"), "admin"), true);
  assert.equal(canGrant(permissionsOf("editor"), "admin"), false);
  assert.equal(canGrant(permissionsOf("editor"), "viewer"), true);
  assert.equal(canGrant(permissionsOf("viewer"), "editor"), false);
  assert.equal(canGrant(permissionsOf("deployer"), "editor"), false);
  assert.equal(canGrant(permissionsOf("editor"), "deployer"), false);
  assert.equal(canGrant(permissionsOf("runtime"), "viewer"), false);
});

test("migration 0103 seed matches permissions.ts (no drift)", () => {
  const sql = readFileSync(fileURLToPath(new URL("../../migrations/0103_auth_seed_roles.sql", import.meta.url)), "utf8");
  const pairs = [...sql.matchAll(/\('([a-z]+)', '([a-z_]+:[a-z_]+)'\)/g)].map((m) => `${m[1]}=${m[2]}`).sort();
  const want = ROLES.flatMap((r) => ROLE_PERMISSIONS[r].map((p) => `${r}=${p}`)).sort();
  assert.deepEqual(pairs, want);
  for (const r of ROLES) assert.ok(sql.includes(`('${r}', true, '${ROLE_DESCRIPTIONS[r].replace(/'/g, "''")}')`));
  const permIds = [...sql.matchAll(/\('([a-z]+:[a-z_]+)', '[a-z]+', '[a-z_]+'\)/g)].map((m) => m[1]!).sort();
  assert.deepEqual(permIds, [...ALL_PERMISSIONS].sort());
});
