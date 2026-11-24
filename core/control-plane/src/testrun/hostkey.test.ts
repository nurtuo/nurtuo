// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { FAKE_NURTUO, j, setup } from "../auth/test-util.js";
import { RunLimiter } from "./semaphore.js";

test("host-key scan checks egress, shares the run limit and persists nothing", async () => {
  const limiter = new RunLimiter();
  const w = setup({ testRun: { bin: FAKE_NURTUO, limiter, lookup: async () => ["8.8.8.8"] } });
  const { ws } = await w.owner("scan@x.io", "ws-scan");
  const session = await w.login("scan@x.io");
  const as = { cookie: session.cookie, csrf: session.csrf };
  const base = `/v1/workspaces/${ws.id}`;
  const scan = (host: string) => w.call("POST", `${base}/connections/host-key-scan`, { ...as, body: { host, port: 22 } });

  const denied = await scan("127.0.0.1");
  assert.equal(denied.status, 400);
  assert.equal(j(denied).error.code, "host_not_allowed");
  const first = limiter.tryAcquire(ws.id)!;
  const second = limiter.tryAcquire(ws.id)!;
  try {
    const capped = await scan("sftp.example");
    assert.equal(capped.status, 429);
    assert.equal(j(capped).error.code, "too_many_runs");
  } finally {
    first();
    second();
  }
  const result = await scan("sftp.example");
  assert.equal(result.status, 200);
  assert.deepEqual(j(result).keys, [{ type: "ssh-ed25519", fingerprintSha256: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }]);
  assert.deepEqual(j(await w.call("GET", `${base}/connections`, as)).items, []);
});
