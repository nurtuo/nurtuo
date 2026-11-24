// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { j, setup } from "../auth/test-util.js";

const VALUE = "super-secret-value-9f3a";

test("secrets are write-only: PUT 204, list shows names only, no route returns a value, audit has no value", async () => {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  const o = await w.login("o@x.io");
  const url = `/v1/workspaces/${ws.id}/secrets`;
  const auth = { cookie: o.cookie, csrf: o.csrf };
  assert.equal((await w.call("PUT", `${url}/db-password`, { ...auth, body: { value: VALUE } })).status, 204);
  const list = await w.call("GET", url, { cookie: o.cookie });
  assert.deepEqual(j(list).items.map((x: any) => Object.keys(x).sort()), [["name", "updatedAt"]]);
  assert.ok(!JSON.stringify(list.body).includes(VALUE));
  const direct = await w.call("GET", `${url}/db-password`, { cookie: o.cookie }); // no such route: fail closed
  assert.equal(direct.status, 403);
  assert.ok(!JSON.stringify(direct.body).includes(VALUE));
  const aud = await w.call("GET", `/v1/workspaces/${ws.id}/audit`, { cookie: o.cookie });
  assert.ok(!JSON.stringify(aud.body).includes(VALUE));
  assert.ok(j(aud).items.some((e: any) => e.action === "secret.put" && e.resourceId === "db-password"));
  assert.equal((await w.call("DELETE", `${url}/db-password`, auth)).status, 204);
  assert.equal((await w.call("DELETE", `${url}/db-password`, auth)).status, 404);
});

test("secret_name_collision and input validation", async () => {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  const o = await w.login("o@x.io");
  const put = (n: string, body: unknown) => w.call("PUT", `/v1/workspaces/${ws.id}/secrets/${n}`, { cookie: o.cookie, csrf: o.csrf, body });
  assert.equal((await put("a-b", { value: "1" })).status, 204);
  const c = await put("a_b", { value: "2" });
  assert.equal(c.status, 409); assert.equal(j(c).error.code, "secret_name_collision");
  assert.equal((await put("ok", { value: "" })).status, 400);
  assert.equal((await put("ok", {})).status, 400);
  assert.equal((await put("-bad", { value: "x" })).status, 400);
});

test("API tokens are refused on secret routes (userOnly)", async () => {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  const { token } = await w.member(ws.id, "ed@x.io", "editor");
  assert.equal((await w.call("PUT", `/v1/workspaces/${ws.id}/secrets/x`, { token: token.raw, body: { value: "v" } })).status, 403);
  assert.equal((await w.call("GET", `/v1/workspaces/${ws.id}/secrets`, { token: token.raw })).status, 403);
});
