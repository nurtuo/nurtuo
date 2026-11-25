// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { j, setup, sftpConn } from "../auth/test-util.js";

test("only a session with CSRF over trusted transport can write credentials, and no response or audit contains their value", async () => {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  const owner = await w.login("o@x.io");
  const { token } = await w.member(ws.id, "ed@x.io", "editor");
  const path = `/v1/workspaces/${ws.id}/connections/c1`;
  const auth = { cookie: owner.cookie, csrf: owner.csrf, ip: "127.0.0.1" };
  const body = sftpConn();
  const put = (options: Parameters<typeof w.call>[2]) => w.call("PUT", path, { body, ...options });
  assert.equal((await put({ token: token.raw })).status, 403);
  assert.equal(j(await put({ cookie: owner.cookie })).error.code, "csrf");
  assert.equal(j(await put({ ...auth, ip: "192.0.2.1", headers: { "x-forwarded-proto": "https" } })).error.code, "tls_required");
  assert.equal((await w.call("GET", path, auth)).status, 404);
  const result = await put(auth);
  assert.equal(result.status, 201);
  assert.equal(j(result).secrets.password, "set");
  for (const response of [result, await w.call("GET", path, auth), await w.call("GET", `/v1/workspaces/${ws.id}/audit`, auth)]) {
    assert.ok(!JSON.stringify(response.body).includes(body.credentials.password));
  }
  const legacy = await w.call("PUT", `/v1/workspaces/${ws.id}/secrets/old`, { ...auth, body: { value: "unused" } });
  assert.equal(j(legacy).error.code, "no_policy");
});

test("trusted proxy must assert HTTPS, even from loopback", async () => {
  const w = setup({ hook: { trustProxy: true } });
  const { ws } = await w.owner("o@x.io", "ws-a");
  const owner = await w.login("o@x.io");
  const put = (proto: string) => w.call("PUT", `/v1/workspaces/${ws.id}/connections/c1`, {
    cookie: owner.cookie, csrf: owner.csrf, headers: { "x-forwarded-proto": proto }, body: sftpConn(),
  });
  assert.equal(j(await put("http")).error.code, "tls_required");
  assert.equal((await put("https")).status, 201);
});
