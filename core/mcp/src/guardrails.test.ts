// SPDX-License-Identifier: AGPL-3.0-only
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { findLiteralSecrets, isSecretKey, redact, REDACTED, withTimeout } from "./guardrails.js";
import { CONNECTORS } from "./catalog.js";

test("isSecretKey recognises credential keys but not descriptors", () => {
  for (const k of ["password", "apiKey", "api_key", "apiKeyValue", "privateKey", "clientToken", "authorization", "nkey_seed", "creds", "passphrase", "secret"]) {
    assert.equal(isSecretKey(k), true, k);
  }
  for (const k of ["apiKeyName", "apiKeyIn", "tokenType", "operation", "connector", "keyboard", "passthrough", "hostKeyPolicy", "baseUrl", "username", "jsonata"]) {
    assert.equal(isSecretKey(k), false, k);
  }
});

test("redact masks secret keys and secret-looking values, keeps references and structure", () => {
  const input = {
    name: "x",
    auth: { type: "bearer", token: "abc123", password: "${secret:db}", apiKeyName: "X-Key", apiKeyValue: "zzz" },
    headers: [{ authorization: "Bearer abcdefghijkl" }],
    url: "postgres://user:s3cret@db:5432/app",
    pem: "-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----",
    n: 5, ok: true, empty: { token: "" },
  };
  const before = JSON.stringify(input);
  const out = redact(input) as typeof input;
  assert.equal(JSON.stringify(input), before, "input is not mutated");
  assert.equal(out.auth.token, REDACTED);
  assert.equal(out.auth.password, "${secret:db}");
  assert.equal(out.auth.apiKeyName, "X-Key");
  assert.equal(out.auth.apiKeyValue, REDACTED);
  assert.equal(out.headers[0]?.authorization, REDACTED);
  assert.equal(out.url, `postgres://user:${REDACTED}@db:5432/app`);
  assert.equal(out.pem, REDACTED);
  assert.equal(out.empty.token, "");
  assert.equal(out.n, 5);
});

test("findLiteralSecrets reports paths", () => {
  assert.deepEqual(findLiteralSecrets({ a: { password: "x" }, b: ["Bearer abcdefghijkl"], c: { password: "${secret:p}" } }), ["/a/password", "/b/0"]);
});

test("withTimeout rejects with a timeout ToolError", async () => {
  await assert.rejects(withTimeout(new Promise(() => {}), 20, "thing"), (e: { code?: string }) => e.code === "timeout");
  assert.equal(await withTimeout(Promise.resolve(1), 20, "thing"), 1);
});

test("connector catalog is in sync with connectors/*/connector.yaml", () => {
  assert.deepEqual(CONNECTORS.map((c) => c.id), ["http", "nats", "postgres", "sftp"]);
  const script = fileURLToPath(new URL("../scripts/gen-connectors.mjs", import.meta.url));
  const r = spawnSync(process.execPath, [script, "--check"], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
});
