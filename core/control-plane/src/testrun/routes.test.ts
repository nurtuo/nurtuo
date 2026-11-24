// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { FAKE_NURTUO, flowOf, j, setup, sftpConn } from "../auth/test-util.js";

async function world(over: Parameters<typeof setup>[0] = {}) {
  const w = setup({ testRun: { bin: FAKE_NURTUO, lookup: async () => ["8.8.8.8"], spoolPollMs: 20, killGraceMs: 300 }, ...over });
  const { ws } = await w.owner("o@x.io", "ws-a");
  await w.member(ws.id, "ed@x.io", "editor");
  await w.member(ws.id, "vw@x.io", "viewer");
  await w.member(ws.id, "dp@x.io", "deployer");
  const ed = await w.login("ed@x.io");
  const as = { cookie: ed.cookie, csrf: ed.csrf };
  await w.call("PUT", `/v1/workspaces/${ws.id}/connections/c1`, { ...as, body: sftpConn() });
  const run = (props: Record<string, string> = {}, who = as) => w.call("POST", `/v1/workspaces/${ws.id}/test-runs`, { ...who, body: { flow: flowOf(), input: { properties: props } } });
  return { ...w, ws, ed, as, run };
}

test("editor runs a flow: 200 with runId, steps and documents", async () => {
  const { run } = await world();
  const r = await run({ mode: "text", text: "hello world" });
  assert.equal(r.status, 200);
  const b = j(r);
  assert.match(b.runId, /^[0-9a-f-]{36}$/);
  assert.equal(b.status, "succeeded");
  assert.equal(Buffer.from(b.documents[0].payload, "base64").toString(), "hello world");
  assert.equal(b.documents[0].size, 11);
  assert.deepEqual(b.steps.map((s: any) => s.status), ["ok", "ok", "ok"]);
});

test("audit: test_run.execute carries runId/status hashes, never output", async () => {
  const w = await world();
  const o = await w.login("o@x.io");
  const r = await w.run({ mode: "text", text: "TOP-SECRET-OUTPUT" });
  const a = j(await w.call("GET", `/v1/workspaces/${w.ws.id}/audit`, { cookie: o.cookie, query: { action: "test_run.execute" } }));
  assert.equal(a.items.length, 1);
  assert.equal(a.items[0].resourceId, j(r).runId);
  assert.ok(!JSON.stringify(a).includes("TOP-SECRET-OUTPUT"));
  assert.equal(a.items[0].status, 200);
});

test("viewer, deployer and API tokens are 403; anonymous is 401", async () => {
  const w = await world();
  for (const e of ["vw@x.io", "dp@x.io"]) { const s = await w.login(e); assert.equal((await w.run({}, { cookie: s.cookie, csrf: s.csrf })).status, 403, e); }
  const { token } = await w.member(w.ws.id, "tk@x.io", "editor");
  assert.equal((await w.call("POST", `/v1/workspaces/${w.ws.id}/test-runs`, { token: token.raw, body: { flow: flowOf() } })).status, 403);
  assert.equal((await w.call("POST", `/v1/workspaces/${w.ws.id}/test-runs`, { body: { flow: flowOf() } })).status, 401);
  const noCsrf = await w.call("POST", `/v1/workspaces/${w.ws.id}/test-runs`, { cookie: w.ed.cookie, body: { flow: flowOf() } });
  assert.equal(noCsrf.status, 403);
});

test("503 test_run_unavailable without a binary", async () => {
  const w = await world({ testRun: { bin: undefined } });
  const r = await w.run();
  assert.equal(r.status, 503); assert.equal(j(r).error.code, "test_run_unavailable");
});

test("third concurrent run in a workspace is 429 with retry-after; the semaphore is released afterwards", async () => {
  const w = await world();
  const dir = mkdtempSync(join(tmpdir(), "pid-"));
  const ac = [new AbortController(), new AbortController()];
  const slow = ac.map((a, i) => w.handle({ method: "POST", path: `/v1/workspaces/${w.ws.id}/test-runs`, ip: "10.0.0.1", signal: a.signal,
    headers: { cookie: `__Host-nurtuo_session=${w.ed.cookie}`, "x-nurtuo-csrf": w.ed.csrf },
    body: { flow: flowOf(), input: { properties: { mode: "sleep", pidfile: join(dir, `p${i}`) } } } }));
  for (let i = 0; i < 200 && !(existsSync(join(dir, "p0")) && existsSync(join(dir, "p1"))); i++) await new Promise((r) => setTimeout(r, 20));
  const third = await w.run({ mode: "echo" });
  assert.equal(third.status, 429);
  assert.equal(j(third).error.code, "too_many_runs");
  assert.equal((third.headers as any)["retry-after"], "5");
  ac.forEach((a) => a.abort());
  await Promise.all(slow);
  assert.equal((await w.run({ mode: "echo" })).status, 200, "slots released");
  assert.ok(readFileSync(join(dir, "p0"), "utf8").length > 0);
});

test("invalid_flow is a 400 with diagnostics; a failed run is still 200", async () => {
  const w = await world();
  const bad = await w.call("POST", `/v1/workspaces/${w.ws.id}/test-runs`, { ...w.as, body: { flow: { schema: "nope" } } });
  assert.equal(bad.status, 400); assert.equal(j(bad).error.code, "invalid_flow"); assert.ok(j(bad).error.diagnostics.length);
  const failed = await w.run({ mode: "fail" });
  assert.equal(failed.status, 200); assert.equal(j(failed).status, "failed");
});

test("network connector Test Run policy checks operations and saved connection type", async () => {
  const w = await world();
  const run = (connector: string, operation: string, connection = "c1") => {
    const flow = flowOf(connection);
    flow.steps[1]!.config = { connector, operation, connection };
    return w.call("POST", `/v1/workspaces/${w.ws.id}/test-runs`, { ...w.as, body: { flow } });
  };

  for (const [connector, operation] of [["http", "CREATE"], ["postgres", "EXECUTE"]]) {
    assert.equal(j(await run(connector!, operation!)).error.code, "not_allowed_in_test_run");
  }
  for (const [connector, operation] of [["http", "GET"], ["postgres", "QUERY"]]) {
    const r = await run(connector!, operation!);
    assert.equal(r.status, 400);
    assert.equal(j(r).error.code, "connection_type_mismatch");
  }
});
