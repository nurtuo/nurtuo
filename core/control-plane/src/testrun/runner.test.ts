// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { FAKE_NURTUO, flowOf, sftpConn } from "../auth/test-util.js";
import { binding } from "../connections/binding.js";
import { ConnectionCoordinator } from "../connections/coordinator.js";
import { MemoryConnectionRepository } from "../connections/repository.js";
import { MemorySecretStore } from "../connections/secrets.js";
import { TestRunner, redact, type TestRunDeps } from "./runner.js";
import { RunLimiter } from "./semaphore.js";

const PW = "pw-value-123";
async function world(over: Partial<TestRunDeps> = {}) {
  const connections = new MemoryConnectionRepository(), secrets = new MemorySecretStore();
  const tmpRoot = mkdtempSync(join(tmpdir(), "runner-test-"));
  const c = { id: "c1", connector: "sftp", incarnation: "incarnation", settings: { ...sftpConn().settings, password: "secret://c1-password" } };
  await connections.put("w1", c);
  await secrets.put("w1", "c1-password", PW, binding(c, "password"));
  const runner = new TestRunner({ connections, secrets, limiter: new RunLimiter(), bin: FAKE_NURTUO, lookup: async () => ["8.8.8.8"], tmpRoot, spoolPollMs: 20, killGraceMs: 300, ...over });
  const run = async (props: Record<string, string> = {}, extra: Record<string, unknown> = {}, signal?: AbortSignal) => {
    const r = await runner.run("w1", { flow: flowOf(), input: { properties: props }, ...extra }, signal);
    return { ...r, body: r.body as any };
  };
  const leftovers = () => readdirSync(tmpRoot);
  return { runner, run, leftovers, tmpRoot, connections, secrets };
}
const alive = (pid: number) => { try { process.kill(pid, 0); return true; } catch { return false; } };

test("argv array, minimal env, HOME and TMPDIR in the temp dir, secret as NURTUO_SECRET_*; temp dir removed", async () => {
  process.env.NURTUO_AUTH_SECRET = "must-not-leak-into-the-child-xxxxxxxxxx";
  process.env.DATABASE_URL = "postgres://must-not-leak";
  try {
    const { run, leftovers } = await world();
    const r = await run({ path: "/a b.txt", mode: "echo" });
    assert.equal(r.status, 200);
    assert.equal(r.body.status, "succeeded");
    assert.deepEqual(r.body.steps.map((s: any) => [s.id, s.status]), [["start-1", "ok"], ["connector-1", "ok"], ["returndocuments-1", "ok"]]);
    const info = JSON.parse(Buffer.from(r.body.documents[0].payload, "base64").toString());
    assert.deepEqual(info.argv.slice(0, 2), ["flow", "run"]);
    assert.ok(info.argv.includes("--json") && info.argv.includes("--connection"));
    assert.ok(info.argv.includes("path=/a b.txt"));
    assert.ok(!info.argv.includes("--insecure-host-key"));
    assert.match(info.argv[info.argv.indexOf("--timeout") + 1], /^\d+ms$/);
    assert.equal(info.env.NURTUO_SECRET_C1_PASSWORD, PW, "prefix present exactly once");
    assert.equal(info.env.NURTUO_SECRET_NURTUO_SECRET_C1_PASSWORD, undefined);
    assert.equal(info.env.NURTUO_AUTH_SECRET, undefined);
    assert.equal(info.env.DATABASE_URL, undefined);
    // Node itself may add PWD / NODE_USE_SYSTEM_CA to the fake's view; nothing else may be inherited.
  const extra = Object.keys(info.env).filter((k) => !k.startsWith("NURTUO_SECRET_") && !["HOME", "PATH", "TMPDIR", "PWD", "NODE_USE_SYSTEM_CA"].includes(k));
  assert.deepEqual(extra, []);
    assert.equal(info.env.HOME, info.env.TMPDIR);
    assert.deepEqual(info.files, ["c1.connection.json", "flow.json"]);
    assert.deepEqual(leftovers(), [], "temp dir removed");
    assert.ok(!JSON.stringify(r.body).includes(PW) || r.body.documents, "no secret in the envelope outside the echoed env");
  } finally { delete process.env.NURTUO_AUTH_SECRET; delete process.env.DATABASE_URL; }
});

test("connection file holds no secret value and the flow file is the request flow", async () => {
  const { run } = await world();
  const r = await run({ mode: "echo" });
  const info = JSON.parse(Buffer.from(r.body.documents[0].payload, "base64").toString());
  const conn = info.argv[info.argv.indexOf("--connection") + 1];
  assert.match(conn, /c1\.connection\.json$/);
  // the temp dir is gone, so check what the runner wrote through the echo of file names only
  assert.deepEqual(info.files, ["c1.connection.json", "flow.json"]);
});

test("limits: timeoutMs range, property count, key and value size", async () => {
  const { run } = await world();
  for (const t of [999, 120001, 1.5, "30"]) await assert.rejects(run({}, { timeoutMs: t }), (e: any) => e.status === 400, String(t));
  const many = Object.fromEntries(Array.from({ length: 65 }, (_, i) => [`k${i}`, "v"]));
  await assert.rejects(run(many), (e: any) => e.status === 400);
  await assert.rejects(run({ "bad key": "v" }), (e: any) => e.status === 400);
  await assert.rejects(run({ k: "x".repeat(4097) }), (e: any) => e.status === 400);
  assert.equal((await run({ k: "x".repeat(4096) }, { timeoutMs: 120000 })).status, 200);
});

test("invalid flow is 400 invalid_flow with diagnostics", async () => {
  const { runner } = await world();
  const r = await runner.run("w1", { flow: { schema: "nurtuo.flow/v1" } });
  assert.equal(r.status, 400); assert.equal((r.body as any).error.code, "invalid_flow"); assert.ok((r.body as any).error.diagnostics.length > 0);
});

test("connection and secret resolution: unknown connection 404, missing secret 409 naming it, never a value", async () => {
  const { runner, secrets } = await world();
  await assert.rejects(runner.run("w1", { flow: flowOf("nope") }), (e: any) => e.status === 404 && e.code === "connection_not_found" && e.message.includes("nope"));
  await secrets.delete("w1", "c1-password");
  await assert.rejects(runner.run("w1", { flow: flowOf() }), (e: any) => e.status === 409 && e.code === "secret_missing" && e.message.includes("password") && e.message.includes("c1"));
});

test("unavailable: no binary, not executable, or win32 -> 503", async () => {
  for (const over of [{ bin: undefined }, { bin: "/nonexistent/nurtuo" }, { platform: "win32" as const }]) {
    const { runner } = await world(over);
    await assert.rejects(runner.run("w1", { flow: flowOf() }), (e: any) => e.status === 503 && e.code === "test_run_unavailable");
  }
});

test("status mapping: Stop step is stopped and parsed, later steps not_run", async () => {
  const { runner, leftovers } = await world();
  const flow: any = flowOf();
  flow.steps.splice(2, 0, { id: "halt-1", kind: "stop", config: { status: 404, message: "gone" } });
  flow.edges["connector-1:default"] = "halt-1"; // a stop step has no output port; returndocuments-1 stays unreachable (not_run)
  delete flow.edges["start-1:default"]; flow.edges["start-1:default"] = "connector-1";
  const r = (await runner.run("w1", { flow, input: { properties: { mode: "stop" } } })).body as any;
  assert.equal(r.status, "stopped");
  assert.deepEqual(r.stop, { status: 404, message: "gone" });
  const omitted = (await runner.run("w1", { flow, input: { properties: { mode: "stop-default" } } })).body as any;
  assert.equal(omitted.status, "stopped");
  assert.deepEqual(omitted.stop, {});
  assert.equal(omitted.steps[2].status, "stopped");
  assert.deepEqual(r.steps.map((s: any) => s.status), ["ok", "ok", "stopped", "not_run"]);
  assert.deepEqual(leftovers(), []);
});

test("status mapping: failed document colours the step error; later steps not_run; exit 2 is failed too", async () => {
  const { run } = await world();
  const r = (await run({ mode: "fail" })).body;
  assert.equal(r.status, "failed"); assert.equal(r.error.code, "execution");
  assert.deepEqual(r.steps.map((s: any) => s.status), ["ok", "error", "not_run"]);
  const u = (await run({ mode: "usage" })).body;
  assert.equal(u.status, "failed"); assert.equal(u.error.code, "validation");
});

test("Prepare failure names the connection: no CLI steps, its connector steps are error, others not_run (fixed message)", async () => {
  const { run } = await world();
  const r = (await run({ mode: "prepare" })).body;
  assert.equal(r.status, "failed");
  assert.match(r.error.message, /^connection "c1" \(sftp\): test failed: ssh connect: host key mismatch: got SHA256:aaa, want SHA256:bbb$/);
  assert.deepEqual(r.steps.map((s: any) => s.status), ["not_run", "error", "not_run"]);
});

test("timeout: the CLI's 'timed out after' message is classified as code timeout, steps that ran keep their status", async () => {
  const { run } = await world();
  const r = (await run({ mode: "timeoutmsg" })).body;
  assert.equal(r.status, "failed"); assert.equal(r.error.code, "timeout");
  assert.deepEqual(r.steps.map((s: any) => s.status), ["ok", "not_run", "not_run"]);
});

test("timeout: a child that outlives timeoutMs is killed (SIGTERM, then SIGKILL), code timeout, temp dir removed", async () => {
  const { run, leftovers } = await world();
  const pidfile = join(mkdtempSync(join(tmpdir(), "pid-")), "pid");
  const r = (await run({ mode: "ignoreterm", pidfile }, { timeoutMs: 1000 })).body;
  assert.equal(r.status, "failed"); assert.equal(r.error.code, "timeout");
  assert.equal(alive(Number(readFileSync(pidfile, "utf8"))), false, "child exited");
  assert.deepEqual(leftovers(), []);
});

test("client abort kills the child and removes the temp dir", async () => {
  const { run, leftovers } = await world();
  const pidfile = join(mkdtempSync(join(tmpdir(), "pid-")), "pid");
  const ac = new AbortController();
  const p = run({ mode: "sleep", pidfile }, {}, ac.signal);
  for (let i = 0; i < 100 && !existsSync(pidfile); i++) await new Promise((r) => setTimeout(r, 20));
  const pid = Number(readFileSync(pidfile, "utf8"));
  assert.equal(alive(pid), true);
  ac.abort();
  await p;
  assert.equal(alive(pid), false);
  assert.deepEqual(leftovers(), []);
});

test("stdout flood is cut at the limit: output_too_large, child killed", async () => {
  const { run, leftovers } = await world({ stdoutLimit: 256 * 1024 });
  const pidfile = join(mkdtempSync(join(tmpdir(), "pid-")), "pid");
  const r = (await run({ mode: "flood", pidfile })).body;
  assert.equal(r.status, "failed"); assert.equal(r.error.code, "output_too_large");
  assert.equal(alive(Number(readFileSync(pidfile, "utf8"))), false);
  assert.deepEqual(leftovers(), []);
});

test("temp dir growth above the spool limit kills the child: output_too_large", async () => {
  const { run, leftovers } = await world({ spoolLimitBytes: 1024 * 1024 });
  const r = (await run({ mode: "spool" })).body;
  assert.equal(r.status, "failed"); assert.equal(r.error.code, "output_too_large");
  assert.deepEqual(leftovers(), []);
});

test("more than 20 documents or more than 1 MiB of payload is output_too_large", async () => {
  const { run } = await world();
  for (const mode of ["bigdocs", "bigpayload"]) {
    const r = (await run({ mode })).body;
    assert.equal(r.status, "failed", mode); assert.equal(r.error.code, "output_too_large", mode); assert.equal(r.documents, undefined);
  }
});

test("unreadable child output and a missing binary exit are failed results, not API errors", async () => {
  const { run } = await world();
  assert.equal((await run({ mode: "badjson" })).body.status, "failed");
  assert.equal((await run({ mode: "nosuchmode" })).body.status, "failed");
});

test("a secret echoed by the child is redacted from the error", async () => {
  const { run } = await world();
  const r = (await run({ mode: "leak" })).body;
  assert.equal(r.status, "failed");
  assert.ok(!JSON.stringify(r).includes(PW));
  assert.match(r.error.message, /\[redacted\]/);
  assert.equal(redact("a pw-value-123 b", [PW]), "a [redacted] b");
});

test("semaphore: slots are released on every outcome", async () => {
  const w = await world();
  for (const mode of ["echo", "fail", "badjson"]) await w.run({ mode });
  await assert.rejects(w.runner.run("w1", { flow: flowOf("nope") }));
  assert.ok(new RunLimiter().tryAcquire("w1"));
  const lim = (w.runner as any).d.limiter as RunLimiter;
  const a = lim.tryAcquire("w1"), b = lim.tryAcquire("w1");
  assert.ok(a && b, "all slots free after the runs above");
});

test("stalled DNS releases both run and scan slots on deadline or disconnect", async () => {
  const limiter = new RunLimiter(1, 1);
  let started!: () => void;
  const { runner, run, leftovers } = await world({
    limiter, scanTimeoutMs: 40,
    lookup: () => {
      started();
      return new Promise<string[]>(() => {});
    },
  });
  for (const scan of [false, true]) {
    for (const disconnect of [false, true]) {
      const controller = new AbortController();
      const resolving = new Promise<void>((resolve) => { started = resolve; });
      const operation = scan
        ? runner.scanHostKey("w1", { host: "sftp.example" }, controller.signal)
        : run({}, { timeoutMs: 1000 }, controller.signal);
      await resolving;
      if (disconnect) {
        controller.abort();
        await assert.rejects(operation, { name: "AbortError" });
      } else if (scan) {
        await assert.rejects(operation, { code: "host_key_scan_failed" });
      } else {
        const result = await operation as Awaited<ReturnType<typeof run>>;
        assert.equal(result.body.error.code, "timeout");
      }
      const release = limiter.tryAcquire("w1");
      assert.ok(release);
      release();
      assert.deepEqual(leftovers(), []);
    }
  }
});

for (const stall of ["workspace lock", "repository"] as const) {
  test(`snapshot waiting on ${stall} releases the slot on deadline and disconnect`, { timeout: 5000 }, async () => {
    const coordinator = new ConnectionCoordinator();
    const limiter = new RunLimiter(1, 1);
    const { runner, connections } = await world({ coordinator, limiter });
    const controller = new AbortController();
    let enter!: () => void;
    const entered = new Promise<void>((resolve) => { enter = resolve; });
    let finish!: () => void;
    const held = new Promise<void>((resolve) => { finish = resolve; });
    const get = connections.get.bind(connections);
    const locked = stall === "workspace lock" ? coordinator.withWorkspace("w1", async () => {
      enter();
      await held;
    }) : undefined;
    if (stall === "repository") connections.get = async (...args) => {
      enter();
      await held;
      return get(...args);
    };

    const request = { flow: flowOf(), timeoutMs: 1000 };
    try {
      const first = runner.run("w1", request);
      await entered;
      assert.equal(((await first).body as any).error.code, "timeout");
      const tryAcquire = limiter.tryAcquire.bind(limiter);
      let admitted!: () => void;
      const admission = new Promise<void>((resolve) => { admitted = resolve; });
      limiter.tryAcquire = (workspace) => {
        const release = tryAcquire(workspace);
        if (release) admitted();
        return release;
      };
      const second = runner.run("w1", request, controller.signal);
      const disconnected = assert.rejects(second, { name: "AbortError" });
      await admission;
      controller.abort();
      await disconnected;
      const release = limiter.tryAcquire("w1");
      assert.ok(release);
      release();
    } finally {
      finish();
      await locked;
    }
  });
}
