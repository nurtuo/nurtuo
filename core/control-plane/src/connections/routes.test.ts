// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { j, setup, sftpConn } from "../auth/test-util.js";

async function world() {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  await w.member(ws.id, "ed@x.io", "editor");
  const ed = await w.login("ed@x.io");
  const put = (id: string, body: unknown) => w.call("PUT", `/v1/workspaces/${ws.id}/connections/${id}`, { cookie: ed.cookie, csrf: ed.csrf, body });
  const get = (path: string) => w.call("GET", `/v1/workspaces/${ws.id}${path}`, { cookie: ed.cookie });
  return { ...w, ws, ed, put, get };
}

test("connection CRUD; secret state is set/missing; settings keep only secret:// references", async () => {
  const { put, get, call, ws, ed } = await world();
  const r = await put("c1", sftpConn());
  assert.equal(r.status, 201);
  assert.equal(j(r).secrets.password, "missing");
  assert.equal((await put("c1", sftpConn({ port: 2222 }))).status, 200);
  assert.equal((await call("PUT", `/v1/workspaces/${ws.id}/secrets/c1-password`, { cookie: ed.cookie, csrf: ed.csrf, body: { value: "pw-value-123" } })).status, 204);
  const list = j(await get("/connections"));
  assert.equal(list.items.length, 1);
  assert.equal(list.items[0].secrets.password, "set");
  assert.equal(list.items[0].settings.password, "secret://c1-password");
  assert.equal(j(await get("/connections/c1")).settings.port, 2222);
  assert.equal((await get("/connections/nope")).status, 404);
  assert.equal((await call("DELETE", `/v1/workspaces/${ws.id}/connections/c1`, { cookie: ed.cookie, csrf: ed.csrf })).status, 204);
  assert.equal((await get("/connections/c1")).status, 404);
});

test("validation codes: literal_secret, insecure_host_key_not_allowed, host_key_required, not_allowed_in_test_run; nothing is stored", async () => {
  const { put, get } = await world();
  const cases: Array<[string, unknown, number, string]> = [
    ["literal password", sftpConn({ password: "hunter2" }), 400, "literal_secret"],
    ["literal private key", sftpConn({ password: undefined, privateKey: "-----BEGIN" }), 400, "literal_secret"],
    ["insecure", sftpConn({ hostKeyPolicy: "insecure" }), 400, "insecure_host_key_not_allowed"],
    ["fingerprint without value", sftpConn({ hostKeyFingerprint: undefined }), 400, "host_key_required"],
    ["known_hosts without content", sftpConn({ hostKeyPolicy: "known_hosts", hostKeyFingerprint: undefined }), 400, "host_key_required"],
    ["default policy without key material", sftpConn({ hostKeyPolicy: undefined, hostKeyFingerprint: undefined }), 400, "host_key_required"],
    ["other connector", { connector: "http", settings: {} }, 400, "not_allowed_in_test_run"],
    ["unknown setting", sftpConn({ bogus: 1 }), 400, "bad_request"],
    ["no credentials", sftpConn({ password: undefined }), 400, "bad_request"],
    ["bad port", sftpConn({ port: 70000 }), 400, "bad_request"],
  ];
  for (const [name, body, status, code] of cases) {
    const r = await put("bad", body);
    assert.equal(r.status, status, name);
    assert.equal(j(r).error.code, code, name);
  }
  assert.equal((await put("Bad_Id", sftpConn())).status, 400);
  assert.deepEqual(j(await get("/connections")).items, []);
});

test("known_hosts policy with content is accepted", async () => {
  const { put } = await world();
  assert.equal((await put("kh", sftpConn({ hostKeyPolicy: "known_hosts", hostKeyFingerprint: undefined, knownHosts: "example.com ssh-ed25519 AAAA" }))).status, 201);
});

test("connection writes are audited (hashes of non-secret settings only)", async () => {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  const o = await w.login("o@x.io");
  await w.call("PUT", `/v1/workspaces/${ws.id}/connections/c1`, { cookie: o.cookie, csrf: o.csrf, body: sftpConn() });
  const a = j(await w.call("GET", `/v1/workspaces/${ws.id}/audit`, { cookie: o.cookie, query: { action: "connection.put" } }));
  assert.equal(a.items.length, 1);
  assert.match(a.items[0].afterHash, /^sha256:/);
  assert.equal(a.items[0].resourceId, "c1");
});
