// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { test } from "node:test";
import { FAKE_NURTUO, flowOf, j, setup, sftpConn } from "../auth/test-util.js";

test("a run in workspace A never uses B's connections or secrets (fake binary records its env)", async () => {
  const w = setup({ testRun: { bin: FAKE_NURTUO, lookup: async () => ["8.8.8.8"] } });
  const { ws: a } = await w.owner("a@x.io", "ws-a");
  const { ws: b } = await w.owner("b@x.io", "ws-b");
  const A = await w.login("a@x.io"), B = await w.login("b@x.io");
  const as = (s: typeof A) => ({ cookie: s.cookie, csrf: s.csrf });
  await w.call("PUT", `/v1/workspaces/${b.id}/connections/only-b`, { ...as(B), body: { ...sftpConn(), credentials: { password: "B-SECRET-VALUE" } } });
  await w.call("PUT", `/v1/workspaces/${a.id}/connections/c1`, { ...as(A), body: { ...sftpConn(), credentials: { password: "A-SECRET-VALUE" } } });

  // A names a connection that exists only in B: the same 404 as for a missing id
  const foreign = await w.call("POST", `/v1/workspaces/${a.id}/test-runs`, { ...as(A), body: { flow: flowOf("only-b") } });
  const missing = await w.call("POST", `/v1/workspaces/${a.id}/test-runs`, { ...as(A), body: { flow: flowOf("does-not-exist") } });
  assert.equal(foreign.status, 404); assert.equal(missing.status, 404);
  assert.equal(j(foreign).error.code, "connection_not_found");
  assert.ok(!JSON.stringify(foreign.body).includes("B-SECRET-VALUE"));
  // A posting to B's workspace path: 404
  assert.equal((await w.call("POST", `/v1/workspaces/${b.id}/test-runs`, { ...as(A), body: { flow: flowOf("only-b") } })).status, 404);

  // A legitimate run reveals only A's secret to the child
  const ok = await w.call("POST", `/v1/workspaces/${a.id}/test-runs`, { ...as(A), body: { flow: flowOf("c1"), input: { properties: { mode: "echo" } } } });
  const info = JSON.parse(Buffer.from(j(ok).documents[0].payload, "base64").toString());
  assert.deepEqual(Object.entries(info.env).filter(([key]) => key.startsWith("NURTUO_SECRET_")).map(([, value]) => value), [createHash("sha256").update("A-SECRET-VALUE").digest("hex")]);
  assert.ok(!JSON.stringify(info).includes("B-SECRET-VALUE"));
  assert.equal(Object.keys(info.env).filter((k) => k.startsWith("NURTUO_SECRET_")).length, 1);
});
