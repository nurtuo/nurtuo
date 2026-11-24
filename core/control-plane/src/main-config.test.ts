// SPDX-License-Identifier: AGPL-3.0-only
// Startup validation of the instance-admin config (spawns dist/main.js; in-memory, no DATABASE_URL).
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const MAIN = fileURLToPath(new URL("./main.js", import.meta.url));
const TIMEOUT_MS = 10_000;

interface Run { code: number | null; stdout: string; stderr: string }

/**
 * Runs main.js with a minimal environment. Resolves when the child exits, or (when `until` matches
 * stderr) kills it and resolves. Always kills the child after TIMEOUT_MS.
 */
function runMain(extra: Record<string, string>, until?: RegExp): Promise<Run> {
  const env: Record<string, string> = { PATH: process.env.PATH ?? "", NURTUO_AUTH_SECRET: "x".repeat(32), PORT: "0", ...extra };
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [MAIN], { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "", done = false;
    const finish = (code: number | null) => { if (done) return; done = true; clearTimeout(timer); resolve({ code, stdout, stderr }); };
    const timer = setTimeout(() => { child.kill("SIGKILL"); if (!done) { done = true; reject(new Error(`main.js timed out\nstdout: ${stdout}\nstderr: ${stderr}`)); } }, TIMEOUT_MS);
    child.stdout.on("data", (d) => { stdout += d; });
    child.stderr.on("data", (d) => {
      stderr += d;
      if (until?.test(stderr)) { child.kill("SIGKILL"); finish(null); }
    });
    child.on("error", (e) => { clearTimeout(timer); if (!done) { done = true; reject(e); } });
    child.on("exit", (code) => finish(code));
  });
}

test("main: NURTUO_ALLOW_WORKSPACE_CREATE=any logs the escape-hatch warning at start", async () => {
  const r = await runMain({ NURTUO_ALLOW_WORKSPACE_CREATE: "any" }, /NURTUO_ALLOW_WORKSPACE_CREATE=any: every session user can create a workspace/);
  assert.match(r.stderr, /NURTUO_ALLOW_WORKSPACE_CREATE=any: every session user can create a workspace and becomes its owner; control-plane data is not partitioned by workspace yet \(#55\)/);
});

test("main: invalid NURTUO_INSTANCE_ADMINS or NURTUO_ALLOW_WORKSPACE_CREATE exits 1 before listening", async () => {
  for (const [env, msg] of [
    [{ NURTUO_INSTANCE_ADMINS: "ops@corp.example,not-an-email" }, /NURTUO_INSTANCE_ADMINS item 2 is not a valid email/],
    [{ NURTUO_ALLOW_WORKSPACE_CREATE: "yes" }, /NURTUO_ALLOW_WORKSPACE_CREATE must be "admins" or "any"/],
  ] as const) {
    const r = await runMain(env);
    assert.equal(r.code, 1, JSON.stringify(env));
    assert.match(r.stderr, msg);
    assert.doesNotMatch(r.stdout, /listening/);
  }
});

test("main: no instance admins warns; with NURTUO_INSECURE_NO_AUTH=1 the variables are not parsed", async () => {
  const none = await runMain({}, /no instance admins configured/);
  assert.match(none.stderr, /no instance admins configured \(NURTUO_INSTANCE_ADMINS, NURTUO_BOOTSTRAP_EMAIL\): POST \/v1\/workspaces is denied for everyone/);
  const insecure = await runMain({ NURTUO_INSECURE_NO_AUTH: "1", NURTUO_ALLOW_WORKSPACE_CREATE: "yes" }, /NURTUO_INSECURE_NO_AUTH=1/);
  assert.equal(insecure.code, null, "still running (killed by the test), not exited 1");
  assert.doesNotMatch(insecure.stderr, /NURTUO_ALLOW_WORKSPACE_CREATE must be/);
});
