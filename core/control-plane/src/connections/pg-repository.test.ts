// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { PgAuthRepository } from "../auth/pg-repository.js";
import { migrate, type Db } from "../pg-repository.js";
import { PgConnectionRepository } from "./pg-repository.js";

const url = process.env.DATABASE_URL;
test("postgres connection repository: migration 0300, workspace-keyed queries, cascade", { skip: !url && "DATABASE_URL not set" }, async () => {
  const mod = "pg";
  const pg = (await import(mod)) as { default: { Pool: new (o: { connectionString: string }) => Db & { end(): Promise<void> } } };
  const schema = `test_connections_${randomBytes(8).toString("hex")}`;
  const admin = new pg.default.Pool({ connectionString: url! });
  const isolated = new URL(url!);
  isolated.searchParams.set("options", `-c search_path=${schema}`);
  const pool = new pg.default.Pool({ connectionString: isolated.toString() });
  try {
    await admin.query(`CREATE SCHEMA ${schema}`);
    await migrate(pool, fileURLToPath(new URL("../../migrations", import.meta.url)));
    const auth = new PgAuthRepository(pool);
    const suffix = Math.random().toString(36).slice(2, 8);
    const u = await auth.createUser({ email: `cr-${suffix}@x.io`, displayName: "", passwordHash: null });
    const wa = await auth.createWorkspace({ slug: `cr-a-${suffix}`, name: "a", ownerId: u.id });
    const wb = await auth.createWorkspace({ slug: `cr-b-${suffix}`, name: "b", ownerId: u.id });
    const repo = new PgConnectionRepository(pool);
    assert.equal((await repo.put(wa.id, { id: "c", connector: "sftp", settings: { host: "h" } })).created, true);
    assert.equal((await repo.put(wa.id, { id: "c", connector: "sftp", settings: { host: "h2" } })).created, false);
    assert.equal(await repo.get(wb.id, "c"), undefined);
    assert.equal((await repo.get(wa.id, "c"))?.settings.host, "h2");
    assert.equal((await repo.list(wa.id)).length, 1);
    await auth.deleteWorkspace(wa.id);
    assert.equal((await pool.query("SELECT count(*)::int AS n FROM connections WHERE workspace_id=$1", [wa.id])).rows[0].n, 0);
    await auth.deleteWorkspace(wb.id);
  } finally {
    await pool.end();
    try { await admin.query(`DROP SCHEMA IF EXISTS ${schema} CASCADE`); }
    finally { await admin.end(); }
  }
});
