// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, generateKeyPairSync } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { j, setup, sftpConn } from "../auth/test-util.js";

async function world() {
  const w = setup();
  const { ws } = await w.owner("o@x.io", "ws-a");
  const owner = await w.login("o@x.io");
  const auth = { cookie: owner.cookie, csrf: owner.csrf, ip: "127.0.0.1" };
  const base = `/v1/workspaces/${ws.id}`;
  const put = (body: unknown) => w.call("PUT", `${base}/connections/c1`, { ...auth, body });
  const get = () => w.call("GET", `${base}/connections/c1`, auth);
  return { ...w, ws, auth, base, put, get };
}

test("settings edits keep the exact credential version and untouched settings", async () => {
  const { put, get } = await world();
  const initial = await put(sftpConn({ timeoutMs: 1234, maxListEntries: 17 }));
  assert.equal(initial.status, 201);
  const ref = j(initial).settings.password;
  assert.match(ref, /^secret:\/\/credential-/);
  assert.equal(j(initial).secrets.password, "set");

  const edit = await put({ connector: "sftp", settings: { rootPath: "/reports" } });
  assert.equal(edit.status, 200);
  assert.deepEqual(j(edit).settings, { ...j(initial).settings, rootPath: "/reports" });

  for (const settings of [{ host: "other.example" }, { port: 2222 }, { user: "other" }, { hostKeyFingerprint: "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB" }]) {
    const rejected = await put({ connector: "sftp", settings });
    assert.equal(rejected.status, 409);
    assert.equal(j(rejected).error.code, "credential_rebind_required");
    assert.deepEqual(j(await get()).settings, j(edit).settings);
  }
  const rotated = await put({ connector: "sftp", settings: { port: 2222 }, credentials: { password: "new-value" } });
  assert.equal(rotated.status, 200);
  assert.equal(j(rotated).settings.port, 2222);
  assert.notEqual(j(rotated).settings.password, ref);
});

test("client references, insecure trust and invalid credential combinations are refused", async () => {
  const { put, get } = await world();
  for (const [body, code] of [
    [sftpConn({ password: "secret://another" }), "secret_ref_not_allowed"],
    [sftpConn({ user: "secret://another" }), "secret_ref_not_allowed"],
    [sftpConn({ hostKeyPolicy: "insecure" }), "insecure_host_key_not_allowed"],
    [sftpConn({ hostKeyFingerprint: undefined }), "host_key_required"],
    [sftpConn({ hostKeyPolicy: "known_hosts" }), "host_key_policy_not_supported"],
    [sftpConn({ bogus: 1 }), "bad_request"],
    [{ ...sftpConn(), credentials: {} }, "invalid_credential_set"],
    [{ ...sftpConn(), credentials: { password: "" } }, "invalid_credential"],
    [{ ...sftpConn(), credentials: { password: "value", passphrase: "value" } }, "invalid_credential_set"],
  ] as const) {
    const r = await put(body);
    assert.equal(r.status, 400);
    assert.equal(j(r).error.code, code);
    assert.equal((await get()).status, 404);
  }

  assert.equal((await put(sftpConn())).status, 201);
  const combination = await put({ connector: "sftp", settings: {}, credentials: { privateKey: "unused" } });
  assert.equal(j(combination).error.code, "credential_rebind_required");
});

test("connection save validates OpenSSH keys and passphrases using the runtime", { skip: !process.env.NURTUO_FLOW_RUN_BIN }, async (t) => {
  const { put, get } = await world();
  const dir = mkdtempSync(join(tmpdir(), "sftp-key-test-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const generateKey = (name: string, passphrase: string) => {
    const path = join(dir, name);
    execFileSync("ssh-keygen", ["-q", "-t", "ed25519", "-N", passphrase, "-f", path]);
    return readFileSync(path, "utf8");
  };
  const passphrase = "test passphrase";
  const plain = generateKey("plain", "");
  const encrypted = generateKey("encrypted", passphrase);
  for (const credentials of [{ privateKey: "invalid" }, { privateKey: encrypted }, { privateKey: encrypted, passphrase: "incorrect" }, { privateKey: plain, passphrase }]) {
    const result = await put({ ...sftpConn(), credentials });
    assert.equal(result.status, 400);
    assert.equal(j(result).error.code, "invalid_credential_set");
    assert.equal((await get()).status, 404);
  }
  const saved = await put({ ...sftpConn(), credentials: { privateKey: encrypted, passphrase } });
  assert.equal(saved.status, 201);
  assert.deepEqual(j(saved).secrets, { privateKey: "set", passphrase: "set" });
  const plainSave = await put({ ...sftpConn(), credentials: { privateKey: plain, passphrase: null } });
  assert.equal(plainSave.status, 200);
  assert.deepEqual(j(plainSave).secrets, { privateKey: "set" });

  // Legacy PEM that the SFTP connector already supports remains usable.
  assert.equal((await put({ ...sftpConn(), credentials: { password: "test password", privateKey: null } })).status, 200);
  const { privateKey } = generateKeyPairSync("ed25519", { privateKeyEncoding: { format: "pem", type: "pkcs8" }, publicKeyEncoding: { format: "pem", type: "spki" } });
  const switched = await put({ connector: "sftp", settings: {}, credentials: { privateKey, password: null } });
  assert.equal(switched.status, 200);
  assert.deepEqual(j(switched).secrets, { privateKey: "set" });
  assert.equal(j(switched).settings.password, undefined);
});

test("a single matching known_hosts key is converted to a fingerprint", async () => {
  const { put } = await world();
  const publicKey = Buffer.concat([Buffer.from([0, 0, 0, 11]), Buffer.from("ssh-ed25519"), Buffer.from([0, 0, 0, 32]), Buffer.alloc(32, 7)]);
  const line = `sftp.example.com ssh-ed25519 ${publicKey.toString("base64")}`;
  const ambiguous = await put(sftpConn({ knownHosts: `${line}\n${line}` }));
  assert.equal(j(ambiguous).error.code, "known_hosts_ambiguous");
  for (const [algorithm, blob] of [["ssh-ed25519", publicKey.subarray(0, -1)], ["ssh-rsa", publicKey]] as const) {
    const invalid = await put(sftpConn({ knownHosts: `sftp.example.com ${algorithm} ${blob.toString("base64")}` }));
    assert.equal(invalid.status, 400);
    assert.equal(j(invalid).error.code, "known_hosts_ambiguous");
  }
  const result = await put(sftpConn({ knownHosts: line }));
  assert.equal(result.status, 201);
  assert.equal(j(result).settings.knownHosts, undefined);
  assert.equal(j(result).settings.hostKeyPolicy, "fingerprint");
  assert.equal(j(result).settings.hostKeyFingerprint, `SHA256:${createHash("sha256").update(publicKey).digest("base64").replace(/=+$/, "")}`);
});

test("ambiguous write outcomes preserve the prior or committed credential generation", async (t) => {
  const { put, get, connections, secrets, ws } = await world();
  const originalWrite = connections.put.bind(connections);
  const originalRead = connections.get.bind(connections);
  assert.equal((await put(sftpConn())).status, 201);
  const prior = j(await get()).settings.password.slice(9);
  const body = { connector: "sftp", settings: { port: 2222 }, credentials: { password: "replacement" } };

  const write = t.mock.method(connections, "put", async () => { throw new Error("write failed"); });
  assert.equal((await put(body)).status, 500);
  assert.equal(j(await get()).settings.password, `secret://${prior}`);
  assert.equal(await secrets.has(ws.id, prior), true);
  assert.equal((await secrets.list(ws.id)).length, 1);

  write.mock.restore();
  const ambiguous = t.mock.method(connections, "put", async (...args: Parameters<typeof originalWrite>) => {
    await originalWrite(...args);
    throw new Error("connection lost after commit");
  });
  assert.equal((await put(body)).status, 200);
  const current = j(await get()).settings.password.slice(9);
  assert.notEqual(current, prior);
  assert.equal(await secrets.has(ws.id, prior), false);

  ambiguous.mock.restore();
  const committedButUnreadable = t.mock.method(connections, "put", async (...args: Parameters<typeof originalWrite>) => {
    await originalWrite(...args);
    t.mock.method(connections, "get", async () => { throw new Error("read unavailable"); });
    throw new Error("connection lost after commit");
  });
  const unknown = await put(body);
  assert.equal(unknown.status, 503);
  assert.equal(j(unknown).error.code, "save_outcome_unknown");
  assert.equal((await secrets.list(ws.id)).length, 2);
  committedButUnreadable.mock.restore();
  connections.get = originalRead;
  const resolved = await get();
  assert.equal(resolved.status, 200);
  assert.equal(j(resolved).secrets.password, "set");
  assert.equal((await secrets.list(ws.id)).length, 1);
});

test("an audit failure preserves the prior connection and credential", async (t) => {
  const { authRepo, put, connections, secrets, ws } = await world();
  assert.equal((await put(sftpConn())).status, 201);
  const before = (await connections.get(ws.id, "c1"))!;
  const prior = String(before.settings.password).slice(9);
  const audit = t.mock.method(authRepo, "appendAudit", async () => { throw new Error("audit unavailable"); });
  const response = await put({ connector: "sftp", settings: { port: 2222 }, credentials: { password: "replacement" } });
  assert.equal(response.status, 500);
  assert.equal(j(response).error.code, "audit_unavailable");
  assert.equal(audit.mock.callCount(), 1);
  assert.deepEqual(await connections.get(ws.id, "c1"), before);
  assert.equal(await secrets.has(ws.id, prior), true);
  assert.equal((await secrets.list(ws.id)).length, 1);
});
