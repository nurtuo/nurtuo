// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { once } from "node:events";
import { createServer, type AddressInfo } from "node:net";
import { test, type TestContext } from "node:test";
import { FAKE_NURTUO, flowOf, j, setup, sftpConn } from "../auth/test-util.js";
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

test("host-key scan denies API tokens and sessions without CSRF before lookup", async (t) => {
  const lookup = t.mock.fn(async () => ["8.8.8.8"]);
  const w = setup({ testRun: { bin: FAKE_NURTUO, lookup } });
  const { user, ws } = await w.owner("scan-session@x.io", "ws-scan-session");
  const token = await w.mint(ws.id, user.id, ["connection:write"]);
  const session = await w.login("scan-session@x.io");
  const endpoint = `/v1/workspaces/${ws.id}/connections/host-key-scan`;
  const body = { host: "sftp.example", port: 22 };
  for (const [auth, code] of [[{ token: token.raw }, "forbidden"], [{ cookie: session.cookie }, "csrf"]] as const) {
    const denied = await w.call("POST", endpoint, { ...auth, body });
    assert.equal(denied.status, 403);
    assert.equal(j(denied).error.code, code);
  }
  assert.equal(lookup.mock.callCount(), 0);
  assert.equal((await w.call("POST", endpoint, { cookie: session.cookie, csrf: session.csrf, body })).status, 200);
  assert.equal(lookup.mock.callCount(), 1);
});

async function dialTarget(t: TestContext, host: string, port = 0) {
  const reached: string[] = [];
  const server = createServer((socket) => {
    reached.push(socket.localAddress!);
    socket.destroy();
  });
  server.listen(port, host);
  await once(server, "listening");
  t.after(() => new Promise<void>((resolve) => server.close(() => resolve())));
  return { port: (server.address() as AddressInfo).port, reached };
}

test("real scan pins its dial to one lookup and a retry uses the new DNS answer", { skip: !process.env.NURTUO_FLOW_RUN_BIN }, async (t) => {
  const first = await dialTarget(t, "127.0.0.1");
  const second = await dialTarget(t, "::1", first.port);
  let answer = ["127.0.0.1", "::1"];
  const lookup = t.mock.fn(async (host: string) => {
    assert.equal(host, "changes.invalid");
    const pinned = answer;
    answer = ["::1", "127.0.0.1"];
    return pinned;
  });
  const w = setup({ testRun: { bin: process.env.NURTUO_FLOW_RUN_BIN, allowPrivate: true, lookup } });
  const { user, ws } = await w.owner("pin@x.io", "ws-pin");
  const session = await w.login("pin@x.io");
  const token = await w.mint(ws.id, user.id, ["connection:write"]);
  const endpoint = `/v1/workspaces/${ws.id}/connections/host-key-scan`;
  const body = { host: "changes.invalid", port: first.port };
  for (const auth of [{ token: token.raw }, { cookie: session.cookie }]) {
    assert.equal((await w.call("POST", endpoint, { ...auth, body })).status, 403);
  }
  assert.deepEqual(first.reached, []);
  assert.deepEqual(second.reached, []);
  assert.equal(lookup.mock.callCount(), 0);
  const scan = () => w.runner.scanHostKey(ws.id, body);
  // The sockets close before speaking SSH. The scan should try only the pinned address.
  await assert.rejects(scan, { code: "host_key_scan_failed" });
  assert.deepEqual(first.reached, ["127.0.0.1"]);
  assert.deepEqual(second.reached, []);
  assert.equal(lookup.mock.callCount(), 1);

  await assert.rejects(scan, { code: "host_key_scan_failed" });
  assert.deepEqual(first.reached, ["127.0.0.1"]);
  assert.deepEqual(second.reached, ["::1"]);
  assert.equal(lookup.mock.callCount(), 2);
});

test("real scan rejects a mixed IPv4/IPv6 answer set with a refused address before dialing", { skip: !process.env.NURTUO_FLOW_RUN_BIN }, async (t) => {
  const local = await dialTarget(t, "127.0.0.1");
  const w = setup({ testRun: { bin: process.env.NURTUO_FLOW_RUN_BIN, lookup: async () => ["2001:db8::1", "127.0.0.1"] } });
  const { ws } = await w.owner("mixed@x.io", "ws-mixed");
  await assert.rejects(w.runner.scanHostKey(ws.id, { host: "mixed.invalid", port: local.port }), { code: "host_not_allowed" });
  assert.deepEqual(local.reached, []);
});

test("real test run dials the pinned address across DNS changes and retries", { skip: !process.env.NURTUO_FLOW_RUN_BIN }, async (t) => {
  const first = await dialTarget(t, "127.0.0.1");
  const second = await dialTarget(t, "::1", first.port);
  let answer = "127.0.0.1";
  const lookup = t.mock.fn(async () => {
    const pinned = answer;
    answer = "::1";
    return [pinned];
  });
  const w = setup({ testRun: { bin: process.env.NURTUO_FLOW_RUN_BIN, allowPrivate: true, lookup } });
  const { ws } = await w.owner("run-pin@x.io", "ws-run-pin");
  const session = await w.login("run-pin@x.io");
  const saved = await w.call("PUT", `/v1/workspaces/${ws.id}/connections/c1`, {
    cookie: session.cookie, csrf: session.csrf, ip: "127.0.0.1",
    body: sftpConn({ host: "changes.invalid", port: first.port }),
  });
  assert.equal(saved.status, 201);
  const run = () => w.runner.run(ws.id, { flow: flowOf(), input: { properties: { path: "/file" } } });
  assert.equal(j(await run()).status, "failed");
  assert.deepEqual(first.reached, ["127.0.0.1"]);
  assert.deepEqual(second.reached, []);
  assert.equal(lookup.mock.callCount(), 1);

  assert.equal(j(await run()).status, "failed");
  assert.deepEqual(first.reached, ["127.0.0.1"]);
  assert.deepEqual(second.reached, ["::1"]);
  assert.equal(lookup.mock.callCount(), 2);
});
