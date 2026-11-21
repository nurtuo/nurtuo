// SPDX-License-Identifier: AGPL-3.0-only
// Postgres-backed auth tests. Skipped unless DATABASE_URL points at a throwaway database
// (the test DROPs the public schema), e.g.
//   docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=pw postgres:16-alpine
//   DATABASE_URL=postgres://postgres:pw@localhost:55432/postgres pnpm test
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { createHandler } from "../api.js";
import { PgRepository, migrate, type Db } from "../pg-repository.js";
import { ROLES, ROLE_PERMISSIONS } from "./permissions.js";
import { createAuthHook } from "./middleware.js";
import { PgAuthRepository } from "./pg-repository.js";
import { AuthService } from "./service.js";

const url = process.env.DATABASE_URL;

test("postgres auth: migrations, seed matches code, constraints, audit immutability, handler smoke", { skip: !url && "DATABASE_URL not set" }, async () => {
  const mod = "pg";
  const pg = (await import(mod)) as { default: { Pool: new (o: { connectionString: string }) => Db & { end(): Promise<void> } } };
  const pool = new pg.default.Pool({ connectionString: url! });
  try {
    await pool.query("DROP SCHEMA public CASCADE; CREATE SCHEMA public");
    const applied = await migrate(pool, fileURLToPath(new URL("../../migrations", import.meta.url)));
    for (const m of ["0100_auth_identity.sql", "0101_auth_tokens_sessions.sql", "0102_audit_log.sql", "0103_auth_seed_roles.sql"]) assert.ok(applied.includes(m), m);

    // seeded matrix == code matrix
    const rows = (await pool.query("SELECT role, permission FROM role_permissions")).rows.map((r) => `${r.role}=${r.permission}`).sort();
    assert.deepEqual(rows, ROLES.flatMap((r) => ROLE_PERMISSIONS[r].map((p) => `${r}=${p}`)).sort());

    const authRepo = new PgAuthRepository(pool);
    const service = new AuthService(authRepo, { secret: randomBytes(32), scrypt: { N: 1024, r: 8, p: 1 }, loginsPerMinutePerIp: 1000 });
    const { user, workspaceId } = await service.bootstrap("Owner@X.io", "correct horse battery staple", "acme");
    assert.equal(user.email, "owner@x.io");
    await assert.rejects(service.createPasswordUser("owner@x.io", "correct horse battery staple"), /already registered/);
    await assert.rejects(pool.query("INSERT INTO users(id,email) VALUES ('u9','Mixed@Case.io')"), "email must be lower-case");
    await assert.rejects(pool.query("INSERT INTO workspaces(id,slug,name) VALUES ('w9','11111111-2222-3333-4444-555555555555','x')"), "uuid-like slug refused");

    // last owner guard + lockout counter
    assert.equal(await authRepo.setMember(workspaceId, user.id, "admin"), "last_owner");
    assert.equal(await authRepo.removeMember(workspaceId, user.id), "last_owner");
    const lock = await (async () => { let r; for (let i = 0; i < 5; i++) r = await authRepo.recordLoginFailure(user.id, 5, 60_000, new Date()); return r!; })();
    assert.ok(lock.lockedUntil);
    await authRepo.recordLoginSuccess(user.id);

    // end-to-end through the handler on Postgres
    const handle = createHandler(new PgRepository(pool), { auth: createAuthHook({ service }) });
    const login = await handle({ method: "POST", path: "/v1/auth/login", body: { email: "owner@x.io", password: "correct horse battery staple" }, headers: {}, ip: "1.2.3.4" });
    assert.equal(login.status, 200);
    const cookie = /__Host-nurtuo_session=([^;]+)/.exec(String((login.headers!["set-cookie"] as string[])[0]))![1]!;
    const csrf = (login.body as any).csrfToken as string;
    const hdr = { cookie: `__Host-nurtuo_session=${cookie}`, "x-nurtuo-csrf": csrf };
    const created = await handle({ method: "POST", path: "/v1/components", body: { name: "orders" }, headers: hdr });
    assert.equal(created.status, 201);
    const tok = await handle({ method: "POST", path: `/v1/workspaces/${workspaceId}/tokens`, body: { name: "ci", scopes: ["component:read"] }, headers: hdr });
    assert.equal(tok.status, 201);
    const viaToken = await handle({ method: "GET", path: "/v1/components", headers: { authorization: `Bearer ${(tok.body as any).token}` } });
    assert.equal(viaToken.status, 200);
    const audit = await handle({ method: "GET", path: `/v1/workspaces/${workspaceId}/audit`, query: { limit: "1" }, headers: hdr });
    assert.equal(audit.status, 200);
    assert.equal((audit.body as any).items.length, 1);
    assert.ok((audit.body as any).nextCursor);

    // audit_log is append-only at the database level
    await assert.rejects(pool.query("UPDATE audit_log SET action='x'"), /append-only/);
    await assert.rejects(pool.query("DELETE FROM audit_log"), /append-only/);
    await assert.rejects(pool.query("TRUNCATE audit_log"), /append-only/);
    assert.ok(((await pool.query("SELECT count(*) AS n FROM audit_log")).rows[0].n as string) !== "0");
    // ... and survives workspace deletion
    const before = (await pool.query("SELECT count(*) AS n FROM audit_log")).rows[0].n;
    await authRepo.deleteWorkspace(workspaceId);
    assert.equal((await pool.query("SELECT count(*) AS n FROM audit_log")).rows[0].n, before);
  } finally { await pool.end(); }
});
