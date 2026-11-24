// SPDX-License-Identifier: AGPL-3.0-only
// Real `nurtuo` binary against a real SFTP server. Skipped unless NURTUO_FLOW_RUN_BIN and the
// NURTUO_TEST_SFTP_* variables are set (the web-e2e job sets them in Phase 6).
import assert from "node:assert/strict";
import { test } from "node:test";
import { flowOf } from "../auth/test-util.js";
import { binding } from "../connections/binding.js";
import { MemoryConnectionRepository } from "../connections/repository.js";
import { MemorySecretStore } from "../connections/secrets.js";
import { TestRunner } from "./runner.js";
import { RunLimiter } from "./semaphore.js";

const e = process.env;
const ready = Boolean(e.NURTUO_FLOW_RUN_BIN && e.NURTUO_TEST_SFTP_HOST && e.NURTUO_TEST_SFTP_PORT && e.NURTUO_TEST_SFTP_USER && e.NURTUO_TEST_SFTP_PASSWORD && e.NURTUO_TEST_SFTP_FINGERPRINT);
const skip = !ready && "NURTUO_FLOW_RUN_BIN / NURTUO_TEST_SFTP_* not set";

async function world(password: string, fingerprint = e.NURTUO_TEST_SFTP_FINGERPRINT!) {
  const connections = new MemoryConnectionRepository(), secrets = new MemorySecretStore();
  const c = { id: "real", connector: "sftp", incarnation: "test-incarnation", settings: { host: e.NURTUO_TEST_SFTP_HOST, port: Number(e.NURTUO_TEST_SFTP_PORT), user: e.NURTUO_TEST_SFTP_USER, password: "secret://real-password", hostKeyPolicy: "fingerprint", hostKeyFingerprint: fingerprint } };
  await connections.put("w", c);
  await secrets.put("w", "real-password", password, binding(c, "password"));
  return new TestRunner({ connections, secrets, limiter: new RunLimiter(), bin: e.NURTUO_FLOW_RUN_BIN, allowPrivate: true });
}

test("real binary: GET returns the file", { skip }, async () => {
  const r = await (await world(e.NURTUO_TEST_SFTP_PASSWORD!)).run("w", { flow: flowOf("real"), input: { properties: { path: e.NURTUO_TEST_SFTP_PATH ?? "/downloads/version.txt" } } });
  const b = r.body as any;
  assert.equal(b.status, "succeeded", JSON.stringify(b));
  assert.deepEqual(b.steps.map((s: any) => s.status), ["ok", "ok", "ok"]);
  assert.ok(Buffer.from(b.documents[0].payload, "base64").length > 0);
});

test("real binary: wrong password fails without leaking it; wrong fingerprint is a host key mismatch", { skip }, async () => {
  const wrong = "definitely-wrong-password-1";
  const r = (await (await world(wrong)).run("w", { flow: flowOf("real"), input: { properties: { path: "/x" } } })).body as any;
  assert.equal(r.status, "failed");
  assert.ok(!JSON.stringify(r).includes(wrong));
  assert.deepEqual(r.steps.map((s: any) => s.status), ["not_run", "error", "not_run"]);
  const m = (await (await world(e.NURTUO_TEST_SFTP_PASSWORD!, "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")).run("w", { flow: flowOf("real"), input: { properties: { path: "/x" } } })).body as any;
  assert.equal(m.status, "failed");
  assert.match(m.error.message, /host key mismatch/);
});
