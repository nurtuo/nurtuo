// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { j, setup, sftpConn } from "../auth/test-util.js";

test("workspace A cannot list, get, put or delete workspace B's connections or secrets; B's data is untouched", async () => {
  const w = setup();
  const { ws: a } = await w.owner("a@x.io", "ws-a");
  const { ws: b } = await w.owner("b@x.io", "ws-b");
  const A = await w.login("a@x.io"), B = await w.login("b@x.io");
  const as = (s: typeof A) => ({ cookie: s.cookie, csrf: s.csrf });
  await w.call("PUT", `/v1/workspaces/${b.id}/connections/secret-conn`, { ...as(B), body: { ...sftpConn(), credentials: { password: "b-only-value" } } });

  // A addressing B's workspace path: 404 (existence is not disclosed)
  for (const [m, p, body] of [
    ["GET", `/v1/workspaces/${b.id}/connections`], ["GET", `/v1/workspaces/${b.id}/connections/secret-conn`],
    ["PUT", `/v1/workspaces/${b.id}/connections/x`, sftpConn()], ["DELETE", `/v1/workspaces/${b.id}/connections/secret-conn`],
  ] as Array<[string, string, unknown?]>) {
    const r = await w.call(m, p, { ...as(A), body });
    assert.equal(r.status, 404, `${m} ${p}`);
  }
  // A's own view: an id that exists only in B is a plain 404, the list is empty, and the secret does not exist
  assert.equal((await w.call("GET", `/v1/workspaces/${a.id}/connections/secret-conn`, { cookie: A.cookie })).status, 404);
  assert.deepEqual(j(await w.call("GET", `/v1/workspaces/${a.id}/connections`, { cookie: A.cookie })).items, []);
  // B is intact
  assert.equal(j(await w.call("GET", `/v1/workspaces/${b.id}/connections`, { cookie: B.cookie })).items.length, 1);
  assert.equal(await w.secrets.reveal(b.id, String((await w.connections.get(b.id, "secret-conn"))?.settings.password).slice(9)), "b-only-value");
});
