// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { MemoryConnectionRepository } from "./repository.js";

test("memory repository is keyed by workspace", async () => {
  const r = new MemoryConnectionRepository();
  const a = await r.put("A", { id: "c", connector: "sftp", settings: { host: "h" } });
  assert.equal(a.created, true);
  assert.equal((await r.put("A", { id: "c", connector: "sftp", settings: { host: "h2" } })).created, false);
  assert.equal(await r.get("B", "c"), undefined);
  assert.deepEqual(await r.list("B"), []);
  assert.equal(await r.delete("B", "c"), false);
  assert.equal((await r.get("A", "c"))?.settings.host, "h2");
  const got = (await r.get("A", "c"))!; got.settings.host = "mutated"; // copies, not live objects
  assert.equal((await r.get("A", "c"))?.settings.host, "h2");
  await r.purge("A");
  assert.deepEqual(await r.list("A"), []);
});
