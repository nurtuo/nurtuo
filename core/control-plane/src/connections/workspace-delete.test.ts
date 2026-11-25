// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { setup, sftpConn } from "../auth/test-util.js";

test("deleting a workspace purges its connections and secrets (memory)", async () => {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  const { ws: other } = await w.owner("p@x.io", "ws-b");
  const o = await w.login("o@x.io");
  const auth = { cookie: o.cookie, csrf: o.csrf, ip: "127.0.0.1" };
  await w.call("PUT", `/v1/workspaces/${ws.id}/connections/c1`, { ...auth, body: sftpConn() });
  const ref = String((await w.connections.get(ws.id, "c1"))?.settings.password).slice(9);
  assert.equal(await w.secrets.has(ws.id, ref), true);
  await w.connections.put(other.id, { id: "keep", connector: "sftp", settings: {} });
  assert.equal((await w.call("DELETE", `/v1/workspaces/${ws.id}`, auth)).status, 204);
  assert.deepEqual(await w.connections.list(ws.id), []);
  assert.equal(await w.secrets.has(ws.id, ref), false);
  assert.equal((await w.connections.list(other.id)).length, 1);
});
