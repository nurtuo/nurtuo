// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { PgRepository, migrate } from "./pg-repository.js";
import { openTestSchema } from "./pg-test-schema.js";

const url = process.env.DATABASE_URL;

test("postgres repository: migrate, revisions, deploy+outbox", { skip: !url && "DATABASE_URL not set" }, async () => {
  const { pool, close } = await openTestSchema(url!);
  try {
    const applied = await migrate(pool, fileURLToPath(new URL("../migrations", import.meta.url)));
    assert.ok(applied.length >= 3); // other feature branches add migrations; auth ones are covered in auth/pg-auth.test.ts
    const repo = new PgRepository(pool);
    const c = await repo.createComponent({ name: "f", kind: "flow", description: "" });
    const r1 = (await repo.createRevision(c.id, { a: 1 }, "sha256:x"))!;
    const r2 = (await repo.createRevision(c.id, { a: 2 }, "sha256:y"))!;
    assert.deepEqual([r1.number, r2.number], [1, 2]);
    await assert.rejects(pool.query("UPDATE revisions SET digest='z'"));
    await repo.upsertRuntime({ id: "rt", name: "n", environment: "dev" });
    const out = await repo.deploy({ componentId: c.id, revisionId: r2.id, runtimeId: "rt" });
    assert.equal(out.desiredState.generation, 1);
    const pending = await repo.listOutbox({ unpublishedOnly: true });
    assert.equal(pending.length, 1);
    assert.equal(await repo.markOutboxPublished(pending[0]!.id), true);
    assert.equal(await repo.markOutboxPublished(pending[0]!.id), false);
    assert.equal((await repo.listOutbox({ unpublishedOnly: true })).length, 0);
    assert.equal((await repo.touchRuntime("rt", "9.9"))!.version, "9.9");
    await repo.markRuntimeOffline("rt");
    assert.equal((await repo.getRuntime("rt"))!.status, "offline");
    assert.equal(await repo.touchRuntime("missing"), undefined);
    const upd = await repo.updateDeploymentStatus(out.deployment.id, "running", "m");
    assert.equal(upd!.status, "running");
  } finally { await close(); }
});
