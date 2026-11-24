// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { MemorySecretStore } from "./secrets.js";
import { ENV_PREFIX, envVar } from "./env-var.js";

// The same fixture is read by the Go test TestEnvVarFixture (core/runtime/internal/secrets).
const fixture: Array<{ name: string; env: string }> = JSON.parse(readFileSync(fileURLToPath(new URL("../../../runtime/internal/secrets/testdata/envvar.json", import.meta.url)), "utf8"));

test("envVar equals the Go fixture and the prefix is present exactly once", () => {
  assert.ok(fixture.length >= 4);
  for (const f of fixture) {
    assert.equal(envVar(f.name), f.env, f.name);
    assert.equal(envVar(f.name).split(ENV_PREFIX).length, 2, f.name);
  }
});

test("names that map to one environment variable collide at write time", async () => {
  const s = new MemorySecretStore();
  await s.put("w", "a-b", "1");
  await assert.rejects(s.put("w", "a_b", "2"), (e: any) => e.status === 409 && e.code === "secret_name_collision");
  await s.put("w", "a-b", "3"); // overwriting the same name is fine
  await s.put("other", "a_b", "4"); // another workspace is independent
});
