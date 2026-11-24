// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { createHash, createPublicKey, randomBytes, verify } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test, type TestContext } from "node:test";
import { loadSigningKey, maxBundleSize, packBundle } from "./bundle.js";

const input = {
  name: "golden",
  createdAt: "2026-11-24T12:34:56.789Z",
  component: { kind: "flow", id: "example", revision: 1, document: { z: 2, a: "hello" } },
};

function keyPath(t: TestContext): string {
  const dir = mkdtempSync(join(tmpdir(), "nurtuo-bundle-test-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return join(dir, "seed");
}

function goldenKey(t: TestContext) {
  const path = keyPath(t);
  const seed = createHash("sha256").update("nurtuo-golden-bundle-key").digest("hex");
  writeFileSync(path, seed, { mode: 0o600 });
  return loadSigningKey(path);
}

test("packBundle matches the Go golden and writes the signed USTAR entries in order", (t) => {
  const key = goldenKey(t);
  const archive = packBundle(input, key);
  assert.deepEqual(packBundle(input, key), archive);
  assert.equal(createHash("sha256").update(archive).digest("hex"), "803cda148d0184722f89de42664a3382dc8f291703d70bd83fe41caacff15d9d");

  const entries = new Map<string, Buffer>();
  let offset = 0;
  for (const name of ["manifest.json", "config.json", "components/flow/example/1.json", "manifest.sig"]) {
    const header = archive.subarray(offset, offset + 512);
    assert.equal(header.toString("ascii", 0, 100).replace(/\0+$/, ""), name);
    assert.equal(header.toString("ascii", 100, 108), "0000644\0");
    assert.equal(header.toString("ascii", 136, 148), "00000000000\0");
    assert.equal(header.toString("ascii", 156, 157), "0");
    assert.equal(header.toString("ascii", 257, 265), "ustar\0" + "00");
    const size = Number.parseInt(header.toString("ascii", 124, 136), 8);
    entries.set(name, archive.subarray(offset + 512, offset + 512 + size));
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  assert.deepEqual(archive.subarray(offset), Buffer.alloc(1024));
  const manifest = entries.get("manifest.json")!;
  const parsed = JSON.parse(manifest.toString());
  assert.deepEqual(Object.keys(parsed), ["format", "name", "createdAt", "components", "config", "connectors"]);
  assert.deepEqual(Object.keys(parsed.components[0]), ["kind", "id", "revision", "path", "sha256", "size"]);
  assert.equal(parsed.createdAt, "2026-11-24T12:34:56Z");
  assert.equal(entries.get("config.json")!.toString(), "{}");
  assert.equal(entries.get("components/flow/example/1.json")!.toString(), '{"a":"hello","z":2}');

  const signature = JSON.parse(entries.get("manifest.sig")!.toString());
  const pub = createPublicKey(key.privateKey);
  assert.equal(signature.alg, "ed25519");
  assert.equal(signature.keyId, createHash("sha256").update(pub.export({ format: "der", type: "spki" }).subarray(-32)).digest("hex").slice(0, 16));
  assert.ok(verify(null, Buffer.concat([Buffer.from("nurtuo-bundle-v1\n"), manifest]), pub, Buffer.from(signature.sig, "base64")));
});

test("packBundle rejects invalid names, revisions and documents", (t) => {
  const key = goldenKey(t);
  for (const name of ["bad/name", "Uppercase", "a".repeat(129), "with\nnewline"]) {
    assert.throws(() => packBundle({ ...input, name }, key), /invalid name/);
  }
  for (const component of [
    { ...input.component, kind: "../flow" },
    { ...input.component, id: ".." },
    { ...input.component, revision: 0 },
  ]) {
    assert.throws(() => packBundle({ ...input, component }, key), /invalid component/);
  }
  for (const document of [null, [], 42]) {
    assert.throws(() => packBundle({ ...input, component: { ...input.component, document } }, key), /must be a JSON object/);
  }
});

test("packBundle includes tar overhead in the 256 MiB cap", (t) => {
  const key = goldenKey(t);
  const document = { payload: "a".repeat(maxBundleSize - 1024) };
  assert.throws(() => packBundle({ ...input, component: { ...input.component, document } }, key), /archive exceeds/);
});

test("loadSigningKey reports invalid and missing files without leaking key contents", (t) => {
  const path = keyPath(t);
  assert.throws(() => loadSigningKey(path), /NURTUO_BUNDLE_SIGNING_KEY_FILE: cannot read/);
  for (const invalid of ["g".repeat(64), randomBytes(31).toString("hex"), randomBytes(33).toString("hex")]) {
    writeFileSync(path, invalid, { mode: 0o600 });
    assert.throws(() => loadSigningKey(path), (error: Error) => {
      assert.match(error.message, /NURTUO_BUNDLE_SIGNING_KEY_FILE: expected a 32-byte hex/);
      assert.ok(!error.message.includes(invalid));
      return true;
    });
  }
});
