// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { createHash, generateKeyPairSync, verify } from "node:crypto";
import { readFileSync } from "node:fs";
import { createServer } from "node:http2";
import type { AddressInfo } from "node:net";
import { after, before, test, type TestContext } from "node:test";
import { fileURLToPath } from "node:url";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { connectNodeAdapter, createGrpcTransport } from "@connectrpc/connect-node";
import { runtimev1 as pb } from "@nurtuo/proto";
import { digestOf } from "./api.js";
import { packBundle } from "./bundle.js";
import { Gateway, createGatewayServer, runtimeIdFromCert } from "./gateway.js";
import { MemoryRepository } from "./memory-repository.js";

const cert = (n: string) => readFileSync(fileURLToPath(new URL(`../testdata/certs/${n}`, import.meta.url)));
const repo = new MemoryRepository();
const gw = new Gateway(repo);
const server = createGatewayServer(gw, { cert: cert("server.pem"), key: cert("server.key"), ca: cert("ca.pem") });
let port = 0;

before(async () => {
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  port = (server.address() as AddressInfo).port;
  for (const id of ["rt-1", "rt-2", "rt-san"]) await repo.upsertRuntime({ id, name: id, environment: "dev" });
});
const h2sessions = new Set<{ destroy(): void }>();
server.on("session", (s) => { h2sessions.add(s); });
after(async () => { gw.closeAll(); for (const s of h2sessions) s.destroy(); await new Promise((r) => server.close(r)); });

/** A runtime-side client: real generated stub over mTLS. */
function runtimeClient(port: number, certName: string, keyName = certName.replace(".pem", ".key"), ca = "ca.pem") {
  return createClient(pb.RuntimeGatewayService, createGrpcTransport({
    baseUrl: `https://localhost:${port}`,
    nodeOptions: { cert: cert(certName), key: cert(keyName), ca: cert(ca) },
  }));
}

class Rt {
  private q: pb.ConnectRequest[] = [];
  private wake?: () => void;
  private done = false;
  private it: AsyncIterator<pb.ConnectResponse>;
  constructor(certName: string, keyName = certName.replace(".pem", ".key"), ca = "ca.pem", address = port) {
    const client = runtimeClient(address, certName, keyName, ca);
    const self = this;
    this.it = client.connect((async function* () {
      for (;;) {
        while (self.q.length) yield self.q.shift()!;
        if (self.done) return;
        await new Promise<void>((r) => { self.wake = r; });
      }
    })())[Symbol.asyncIterator]();
  }
  send(msg: pb.ConnectRequest["msg"]) { this.q.push(create(pb.ConnectRequestSchema, { msg })); this.wake?.(); }
  hello(id: string, rev = 0) { this.send({ case: "hello", value: create(pb.HelloSchema, { runtimeId: id, version: "1.2.3", desiredStateRevision: BigInt(rev) }) }); }
  private pending?: Promise<IteratorResult<pb.ConnectResponse>>;
  private pull() {
    if (!this.pending) { this.pending = this.it.next(); const clear = () => { this.pending = undefined; }; this.pending.then(clear, clear); }
    return this.pending;
  }
  async next(ms = 2000): Promise<pb.ConnectResponse> {
    const t = new Promise<never>((_, rej) => setTimeout(() => rej(new Error("timeout waiting for message")), ms).unref());
    const r = await Promise.race([this.pull(), t]);
    assert.ok(!r.done, "stream ended");
    return r.value;
  }
  async desired(): Promise<pb.DesiredState> {
    const m = (await this.next()).msg;
    assert.equal(m.case, "desiredState");
    return m.value as pb.DesiredState;
  }
  close() { this.done = true; this.wake?.(); }
}

async function seed(name: string) {
  const c = await repo.createComponent({ name, kind: "flow", description: "" });
  const r = (await repo.createRevision(c.id, { n: name }, `sha256:${name}`))!;
  return { c, r };
}
async function eventually(fn: () => Promise<boolean>, ms = 2000) {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (await fn()) return; await new Promise((r) => setTimeout(r, 20)); }
  assert.fail("condition not met in time");
}

test("optional bundle signing advertises stable archive digests for derived revisions", async (t) => {
  const repo = new MemoryRepository();
  await repo.upsertRuntime({ id: "rt", name: "rt", environment: "dev" });
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const rawPublicKey = publicKey.export({ type: "spki", format: "der" }).subarray(-32);
  const signingKey = { privateKey, keyId: createHash("sha256").update(rawPublicKey).digest("hex").slice(0, 16) };

  const derived = await repo.createComponent({ name: "Display Name", kind: "flow", description: "" });
  const content = { z: 1, a: "flow" };
  const derivedRev = (await repo.createRevision(derived.id, content, digestOf(content)))!;
  await repo.deploy({ componentId: derived.id, revisionId: derivedRev.id, runtimeId: "rt" });

  const external = await repo.createComponent({ name: "external", kind: "flow", description: "" });
  const externalDigest = `sha256:${"a".repeat(64)}`;
  const externalRev = (await repo.createRevision(external.id, {}, externalDigest))!;
  await repo.deploy({ componentId: external.id, revisionId: externalRev.id, runtimeId: "rt" });

  const invalid = await repo.createComponent({ name: "invalid", kind: "flow", description: "" });
  const invalidRev = (await repo.createRevision(invalid.id, [], digestOf([])))!;
  await repo.deploy({ componentId: invalid.id, revisionId: invalidRev.id, runtimeId: "rt" });

  const expected = packBundle({
    name: derived.id,
    createdAt: derivedRev.createdAt,
    component: { kind: "flow", id: derived.id, revision: derivedRev.number, document: content },
  }, signingKey);
  const archiveDigest = `sha256:${createHash("sha256").update(expected).digest("hex")}`;
  assert.notEqual(archiveDigest, derivedRev.digest);

  async function digests(gateway: Gateway) {
    const message = await gateway.desiredStateMessage("rt");
    return Object.fromEntries(message!.deployments.map(({ deploymentId, bundleDigest }) => [deploymentId, bundleDigest]));
  }
  const legacy = { [derived.id]: derivedRev.digest, [external.id]: externalDigest, [invalid.id]: invalidRev.digest };
  assert.deepEqual(await digests(new Gateway(repo)), legacy);
  const lookup = t.mock.method(repo, "getComponent");
  const signing = new Gateway(repo, { signingKey });
  const signed = { ...legacy, [derived.id]: archiveDigest };
  assert.deepEqual(await digests(signing), signed);
  assert.deepEqual(await digests(signing), signed);
  // The invalid revision is retried, the derived revision is cached, and the external one is skipped.
  assert.equal(lookup.mock.callCount(), 3);
  assert.deepEqual(await digests(new Gateway(repo, { signingKey })), signed);
});

async function listen(t: TestContext, server: ReturnType<typeof createGatewayServer> | ReturnType<typeof createServer>) {
  const sessions = new Set<{ destroy(): void }>();
  server.on("session", (session) => {
    sessions.add(session);
    session.once("close", () => sessions.delete(session));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    for (const session of sessions) session.destroy();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  });
  return (server.address() as AddressInfo).port;
}

test("matching-generation reconnects receive current digests across signing changes", async (t) => {
  const repo = new MemoryRepository();
  const runtimeId = "rt-1";
  await repo.upsertRuntime({ id: runtimeId, name: runtimeId, environment: "dev" });
  const component = await repo.createComponent({ name: "resume", kind: "flow", description: "" });
  const content = { a: "flow" };
  const revision = (await repo.createRevision(component.id, content, digestOf(content)))!;
  await repo.deploy({ componentId: component.id, revisionId: revision.id, runtimeId });
  const { privateKey } = generateKeyPairSync("ed25519");
  const signingKey = { privateKey, keyId: "0123456789abcdef" };

  async function reconnect(signed: boolean) {
    const address = await listen(t, createGatewayServer(new Gateway(repo, { signingKey: signed ? signingKey : undefined }), {
      cert: cert("server.pem"), key: cert("server.key"), ca: cert("ca.pem"),
    }));
    const rt = new Rt("rt-1.pem", "rt-1.key", "ca.pem", address);
    try {
      rt.hello(runtimeId, 1);
      const desired = await rt.desired();
      assert.equal(desired.revision, 1n);
      return desired.deployments[0]!.bundleDigest;
    } finally {
      rt.close();
    }
  }

  assert.equal(await reconnect(false), revision.digest);
  assert.notEqual(await reconnect(true), revision.digest);
  assert.equal(await reconnect(false), revision.digest);
});

const fetchClient = (client: ReturnType<typeof runtimeClient>) => async (digest: string) => {
  const chunks: Uint8Array[] = [];
  for await (const chunk of client.fetchArtifact({ digest }, { timeoutMs: 5000 })) chunks.push(chunk.data);
  return chunks;
};

test("FetchArtifact serves signed, bounded chunks only to a runtime that currently needs the digest", async (t) => {
  const repo = new MemoryRepository();
  for (const id of ["rt-1", "rt-2"]) await repo.upsertRuntime({ id, name: id, environment: "dev" });
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const publicKeyBytes = publicKey.export({ type: "spki", format: "der" }).subarray(-32);
  const keyId = createHash("sha256").update(publicKeyBytes).digest("hex").slice(0, 16);
  async function deploy(name: string, content: unknown, digest = digestOf(content)) {
    const component = await repo.createComponent({ name, kind: "flow", description: "" });
    const revision = (await repo.createRevision(component.id, content, digest))!;
    await repo.deploy({ componentId: component.id, revisionId: revision.id, runtimeId: "rt-1" });
    return component.id;
  }
  const content = { large: "x".repeat(1024 * 1024) };
  const componentId = await deploy("derived", content);
  const external = `sha256:${"a".repeat(64)}`;
  await deploy("external", {}, external);
  await deploy("invalid", []);

  const opts = { signingKey: { privateKey, keyId } };
  const gateway = new Gateway(repo, opts);
  const state = await gateway.desiredStateMessage("rt-1");
  const digest = state!.deployments.find((d) => d.deploymentId === componentId)!.bundleDigest;
  assert.notEqual(digest, digestOf(content));

  const address = await listen(t, createGatewayServer(gateway, {
    cert: cert("server.pem"), key: cert("server.key"), ca: cert("ca.pem"),
  }));
  const owner = runtimeClient(address, "rt-1.pem");
  const fetch = fetchClient(owner);
  const chunks = await fetch(digest.slice("sha256:".length));
  assert.ok(chunks.length > 1);
  assert.ok(chunks.every((chunk) => chunk.length > 0 && chunk.length <= 1024 * 1024));
  const archive = Buffer.concat(chunks);
  assert.equal(`sha256:${createHash("sha256").update(archive).digest("hex")}`, digest);
  const entries = new Map<string, Buffer>();
  for (let offset = 0; archive[offset] !== 0;) {
    const name = archive.toString("ascii", offset, offset + 100).split("\0")[0]!;
    const size = parseInt(archive.toString("ascii", offset + 124, offset + 136), 8);
    entries.set(name, archive.subarray(offset + 512, offset + 512 + size));
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  const manifest = entries.get("manifest.json")!;
  const signature = JSON.parse(entries.get("manifest.sig")!.toString());
  assert.equal(signature.keyId, keyId);
  assert.equal(signature.alg, "ed25519");
  assert.ok(verify(null, Buffer.concat([Buffer.from("nurtuo-bundle-v1\n"), manifest]), publicKey, Buffer.from(signature.sig, "base64")));

  const notFound = { code: Code.NotFound, rawMessage: "artifact not found for this runtime" };
  const fetchOther = fetchClient(runtimeClient(address, "rt-2.pem"));
  await assert.rejects(fetchOther(digest), notFound);
  await assert.rejects(fetchOther(`sha256:${"b".repeat(64)}`), notFound);
  await assert.rejects(fetch(digestOf(content)), notFound);
  for (const malformed of ["sha256:bad", digest.toUpperCase(), `${digest}\n`]) {
    await assert.rejects(fetch(malformed), { code: Code.InvalidArgument });
  }
  await assert.rejects(fetch(external), {
    code: Code.NotFound, rawMessage: "artifact is not stored by the control plane; place it in bundle_dir",
  });
  await assert.rejects(fetch(digestOf([])), {
    code: Code.FailedPrecondition, rawMessage: "bundle: component document must be a JSON object",
  });
  await assert.rejects(fetchClient(runtimeClient(address, "rogue.pem"))(digest),
    (error) => error instanceof ConnectError && [Code.Canceled, Code.Unavailable].includes(error.code));
  const cancel = new AbortController();
  const stream = owner.fetchArtifact({ digest }, { signal: cancel.signal, timeoutMs: 5000 })[Symbol.asyncIterator]();
  assert.equal((await stream.next()).done, false);
  cancel.abort();
  await assert.rejects(stream.next(), { code: Code.Canceled });

  const revised = (await repo.createRevision(componentId, {}, digestOf({})))!;
  await repo.deploy({ componentId, revisionId: revised.id, runtimeId: "rt-1" });
  await assert.rejects(fetch(digest), notFound);
});

test("FetchArtifact repacks only an authorized cache miss", async (t) => {
  const repo = new MemoryRepository();
  for (const id of ["rt-1", "rt-2"]) await repo.upsertRuntime({ id, name: id, environment: "dev" });
  for (const name of ["one", "two", "three"]) {
    const component = await repo.createComponent({ name, kind: "flow", description: "" });
    const content = { name };
    const revision = (await repo.createRevision(component.id, content, digestOf(content)))!;
    await repo.deploy({ componentId: component.id, revisionId: revision.id, runtimeId: "rt-1" });
  }
  const { privateKey } = generateKeyPairSync("ed25519");
  const gateway = new Gateway(repo, { signingKey: { privateKey, keyId: "0123456789abcdef" }, artifactCacheBytes: 5120 });
  const address = await listen(t, createGatewayServer(gateway, {
    cert: cert("server.pem"), key: cert("server.key"), ca: cert("ca.pem"),
  }));
  const rt = new Rt("rt-1.pem", "rt-1.key", "ca.pem", address);
  t.after(() => rt.close());
  rt.hello("rt-1");
  const desired = await rt.desired();
  const digest = desired.deployments[0]!.bundleDigest;
  const lookup = t.mock.method(repo, "getComponent");
  const fetch = fetchClient(runtimeClient(address, "rt-1.pem"));
  const foreignFetch = fetchClient(runtimeClient(address, "rt-2.pem"));
  const notFound = { code: Code.NotFound, rawMessage: "artifact not found for this runtime" };
  await assert.rejects(foreignFetch(digest), notFound);
  await assert.rejects(fetch(`sha256:${"b".repeat(64)}`), notFound);
  assert.equal(lookup.mock.callCount(), 0);
  const archives = await Promise.all([fetch(digest), fetch(digest)]);
  for (const chunks of archives) {
    assert.equal(`sha256:${createHash("sha256").update(Buffer.concat(chunks)).digest("hex")}`, digest);
  }
  assert.equal(lookup.mock.callCount(), 1);
  assert.deepEqual(lookup.mock.calls[0]!.arguments, [desired.deployments[0]!.deploymentId]);
});

test("FetchArtifact fails closed without a signing key or without a certificate identity", async (t) => {
  const digest = digestOf({});
  const repo = new MemoryRepository();
  await repo.upsertRuntime({ id: "rt-1", name: "rt-1", environment: "dev" });
  const component = await repo.createComponent({ name: "unsigned", kind: "flow", description: "" });
  const revision = (await repo.createRevision(component.id, {}, digest))!;
  await repo.deploy({ componentId: component.id, revisionId: revision.id, runtimeId: "rt-1" });
  const gateway = new Gateway(repo);
  const secureAddress = await listen(t, createGatewayServer(gateway, {
    cert: cert("server.pem"), key: cert("server.key"), ca: cert("ca.pem"),
  }));
  await assert.rejects(fetchClient(runtimeClient(secureAddress, "rt-1.pem"))(digest), {
    code: Code.FailedPrecondition,
    rawMessage: "control plane has no bundle signing key configured (NURTUO_BUNDLE_SIGNING_KEY_FILE)",
  });

  const address = await listen(t, createServer(connectNodeAdapter({ routes: (router) => gateway.register(router) })));
  const anonymous = createClient(pb.RuntimeGatewayService, createGrpcTransport({ baseUrl: `http://127.0.0.1:${address}` }));
  await assert.rejects(fetchClient(anonymous)(digest), {
    code: Code.Unauthenticated, rawMessage: "client certificate carries no runtime identity",
  });
});

test("runtimeIdFromCert prefers URI SAN over CN", () => {
  assert.equal(runtimeIdFromCert({ subject: { CN: "x" } }), "x");
  assert.equal(runtimeIdFromCert({ subject: { CN: "decoy" }, subjectaltname: "DNS:a, URI:spiffe://nurtuo/runtime/r9" }), "r9");
  assert.equal(runtimeIdFromCert(undefined), undefined);
});

test("runtimeIdFromCert: standalone client cert (CN nurtuo-runtime + URI SAN) is runtime 'standalone' (#56)", () => {
  // Shape node reports for the cert issued by `nurtuo standalone` (standalone_bootstrap.go).
  const standalone = {
    subject: { CN: "nurtuo-runtime" },
    subjectaltname: "DNS:localhost, IP Address:127.0.0.1, IP Address:0:0:0:0:0:0:0:1, URI:spiffe://nurtuo/runtime/standalone",
  };
  assert.equal(runtimeIdFromCert(standalone), "standalone");
  // Without the SAN (installs from before #56) the identity stays the CN, so Hello "standalone" is refused.
  assert.equal(runtimeIdFromCert({ subject: { CN: "nurtuo-runtime" }, subjectaltname: "DNS:localhost" }), "nurtuo-runtime");
});

test("happy path: Hello reconciles, status persisted, heartbeat tracked", async () => {
  const { c, r } = await seed("happy");
  const { deployment } = await repo.deploy({ componentId: c.id, revisionId: r.id, runtimeId: "rt-1" });
  await gw.publishOutbox(); // not connected yet: row consumed, Hello reconcile delivers
  const rt = new Rt("rt-1.pem");
  rt.hello("rt-1", 0);
  const ds = await rt.desired();
  assert.equal(ds.revision, 1n);
  assert.equal(ds.deployments.length, 1);
  assert.equal(ds.deployments[0]!.deploymentId, c.id);
  assert.equal(ds.deployments[0]!.bundleDigest, "sha256:happy");
  assert.equal((await repo.getRuntime("rt-1"))!.version, "1.2.3");
  assert.deepEqual(gw.connectedRuntimes(), ["rt-1"]);

  rt.send({ case: "deploymentStatus", value: create(pb.DeploymentStatusSchema, { revision: 1n, deploymentId: c.id, state: "running", message: "ok" }) });
  await eventually(async () => (await repo.getDeployment(deployment.id))!.status === "running");
  assert.equal((await repo.getDeployment(deployment.id))!.message, "ok");
  rt.send({ case: "deploymentStatus", value: create(pb.DeploymentStatusSchema, { revision: 1n, deploymentId: c.id, state: "error", message: "boom" }) });
  await eventually(async () => (await repo.getDeployment(deployment.id))!.status === "failed");

  // A re-apply is reported at the current generation (a sibling deploy moved it to 2): it must
  // still update the component's newest deployment row.
  const sib = await seed("happy-sibling");
  await repo.deploy({ componentId: sib.c.id, revisionId: sib.r.id, runtimeId: "rt-1" }); // generation 2
  for (const row of await repo.listOutbox({ unpublishedOnly: true })) await repo.markOutboxPublished(row.id); // keep the stream quiet
  rt.send({ case: "deploymentStatus", value: create(pb.DeploymentStatusSchema, { revision: 2n, deploymentId: c.id, state: "running", message: "retried" }) });
  await eventually(async () => (await repo.getDeployment(deployment.id))!.message === "retried");
  assert.equal((await repo.getDeployment(deployment.id))!.status, "running");

  const before = (await repo.getRuntime("rt-1"))!.lastSeenAt!;
  await new Promise((r) => setTimeout(r, 15));
  rt.send({ case: "heartbeat", value: create(pb.HeartbeatSchema, {}) });
  await eventually(async () => (await repo.getRuntime("rt-1"))!.lastSeenAt! > before);

  // command round trip
  const p = gw.sendCommand("rt-1", "pause_listener", { id: "l1" });
  const cmd = (await rt.next()).msg;
  assert.equal(cmd.case, "command");
  assert.equal((cmd.value as pb.Command).name, "pause_listener");
  rt.send({ case: "commandResult", value: create(pb.CommandResultSchema, { commandId: (cmd.value as pb.Command).commandId, ok: true }) });
  assert.equal((await p).ok, true);

  rt.close();
  await eventually(async () => (await repo.getRuntime("rt-1"))!.status === "offline");
  assert.deepEqual(gw.connectedRuntimes(), []);
});

test("Hello whose runtime_id does not match the certificate is rejected", async () => {
  const rt = new Rt("rt-1.pem");
  rt.hello("rt-2");
  await assert.rejects(rt.next(), (e) => e instanceof ConnectError && e.code === Code.PermissionDenied);
  assert.deepEqual(gw.connectedRuntimes(), []);
});

test("first message must be Hello", async () => {
  const a = new Rt("rt-2.pem");
  a.send({ case: "heartbeat", value: create(pb.HeartbeatSchema, {}) });
  await assert.rejects(a.next(), (e) => e instanceof ConnectError && e.code === Code.InvalidArgument);
});

test("identity can come from a URI SAN (CN ignored)", async () => {
  const rt = new Rt("rt-san.pem");
  rt.hello("decoy");
  await assert.rejects(rt.next(), (e) => e instanceof ConnectError && e.code === Code.PermissionDenied);
  const ok = new Rt("rt-san.pem");
  ok.hello("rt-san");
  await eventually(async () => gw.connectedRuntimes().includes("rt-san"));
  ok.close();
});

test("client cert from an untrusted CA is refused at the TLS layer", async () => {
  const rt = new Rt("rogue.pem");
  rt.hello("rt-1");
  await assert.rejects(rt.next());
});

test("desired state is pushed after deploy() via the outbox; matching revision is replayed", async () => {
  const { c, r } = await seed("push");
  await repo.deploy({ componentId: c.id, revisionId: r.id, runtimeId: "rt-2" });
  await gw.publishOutbox();
  const rt = new Rt("rt-2.pem");
  rt.hello("rt-2", 1); // already at current revision
  assert.equal((await rt.desired()).revision, 1n);
  const c2 = await seed("push2");
  await repo.deploy({ componentId: c2.c.id, revisionId: c2.r.id, runtimeId: "rt-2" });
  assert.equal((await repo.listOutbox({ unpublishedOnly: true })).length, 1);
  assert.equal(await gw.publishOutbox(), 1);
  const ds = await rt.desired();
  assert.equal(ds.revision, 2n);
  assert.deepEqual(ds.deployments.map((d) => d.bundleDigest).sort(), ["sha256:push", "sha256:push2"]);
  assert.equal((await repo.listOutbox({ unpublishedOnly: true })).length, 0);
  assert.equal(await gw.publishOutbox(), 0); // idempotent
  rt.close();
});

test("reconnect resends state missed while offline", async () => {
  await eventually(async () => !gw.connectedRuntimes().includes("rt-2"));
  const { c, r } = await seed("offline");
  await repo.deploy({ componentId: c.id, revisionId: r.id, runtimeId: "rt-2" }); // generation 3
  await gw.publishOutbox();
  const rt = new Rt("rt-2.pem");
  rt.hello("rt-2", 2);
  const ds = await rt.desired();
  assert.equal(ds.revision, 3n);
  assert.equal(ds.deployments.length, 3);
  rt.close();
});

test("a newer connection for the same runtime replaces the older one", async () => {
  const a = new Rt("rt-1.pem");
  a.hello("rt-1", 2);
  assert.equal((await a.desired()).revision, 2n);
  const b = new Rt("rt-1.pem");
  b.hello("rt-1", 2);
  await a.next().then(() => assert.fail("expected stream end"), () => {});
  assert.deepEqual(gw.connectedRuntimes(), ["rt-1"]);
  a.close();
  b.close();
  await eventually(async () => gw.connectedRuntimes().length === 0);
  await new Promise((r) => setTimeout(r, 50));
});

test("Hello replay cannot send an older generation after an outbox push", { timeout: 5000 }, async (t) => {
  const runtimeId = "rt-san";
  const { c, r } = await seed("hello-outbox-order");
  const initial = await repo.deploy({ componentId: c.id, revisionId: r.id, runtimeId });
  await gw.publishOutbox();
  const getDesiredState = repo.getDesiredState.bind(repo);
  let signalSnapshot!: () => void;
  const snapshotRead = new Promise<void>((resolve) => { signalSnapshot = resolve; });
  let releaseHello!: () => void;
  const resumeHello = new Promise<void>((resolve) => { releaseHello = resolve; });
  t.mock.method(repo, "getDesiredState", async (id: string) => {
    const snapshot = await getDesiredState(id);
    signalSnapshot();
    await resumeHello;
    return snapshot;
  }, { times: 1 });

  const rt = new Rt("rt-san.pem");
  try {
    rt.hello(runtimeId);
    await snapshotRead;
    const next = (await repo.createRevision(c.id, { updated: true }, "sha256:updated"))!;
    const deployment = await repo.deploy({ componentId: c.id, revisionId: next.id, runtimeId });
    assert.equal(deployment.desiredState.generation, initial.desiredState.generation + 1);
    await gw.publishOutbox();
    assert.equal((await rt.desired()).revision, BigInt(deployment.desiredState.generation));
    releaseHello();
    // The duplicate Hello ends the stream after the replay has had a chance to enqueue.
    rt.hello(runtimeId);
    await assert.rejects(rt.next(), { code: Code.InvalidArgument });
  } finally {
    releaseHello();
    rt.close();
  }
});
