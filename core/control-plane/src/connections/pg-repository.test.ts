// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { PgAuthRepository } from "../auth/pg-repository.js";
import { migrate, type Db } from "../pg-repository.js";
import { PgConnectionRepository } from "./pg-repository.js";
import { openTestSchema } from "../pg-test-schema.js";

const url = process.env.DATABASE_URL;
test("postgres connection repository: migration 0300, workspace-keyed queries, cascade", { skip: !url && "DATABASE_URL not set" }, async () => {
  const { pool, close } = await openTestSchema(url!);
  try {
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
    await assert.rejects(repo.put(wa.id, { id: "c", connector: "sftp", settings: { host: "rolled-back" } }, async () => {
      throw new Error("audit unavailable");
    }), /audit unavailable/);
    assert.equal((await repo.get(wa.id, "c"))?.settings.host, "h2");
    await repo.put(wa.id, { id: "c", connector: "sftp", settings: { host: "audited" } }, async (created, append) => {
      assert.equal(created, false);
      assert.ok(append);
      await append({
        workspaceId: wa.id, actorType: "user", actorId: u.id, action: "connection.put",
        resource: "connection", resourceId: "c", beforeHash: null, afterHash: null,
        ip: null, requestId: "pg-connection-test", status: 200, metadata: {},
      });
    });
    assert.equal((await repo.get(wa.id, "c"))?.settings.host, "audited");
    assert.equal((await pool.query("SELECT status FROM audit_log WHERE request_id=$1", ["pg-connection-test"])).rows[0]?.status, 200);
    await auth.deleteWorkspace(wa.id);
    assert.equal((await pool.query("SELECT count(*)::int AS n FROM connections WHERE workspace_id=$1", [wa.id])).rows[0].n, 0);
    await auth.deleteWorkspace(wb.id);
  } finally { await close(); }
});

test("rollback failure preserves the original transaction error", async () => {
  const original = new Error("connection lost during write");
  let released = false;
  const db: Db = {
    query: async () => { throw new Error("unexpected pool query"); },
    connect: async () => ({
      query: async (sql) => {
        if (sql === "BEGIN") return { rows: [], rowCount: null };
        if (sql === "ROLLBACK") throw new Error("connection lost during rollback");
        throw original;
      },
      release: () => { released = true; },
    }),
  };
  const repo = new PgConnectionRepository(db);
  await assert.rejects(
    repo.put("w", { id: "c", connector: "sftp", settings: {} }, async () => {}),
    (error) => error === original,
  );
  assert.equal(released, true);
});
