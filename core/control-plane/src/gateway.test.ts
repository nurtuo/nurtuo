// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { createHash, generateKeyPairSync } from "node:crypto";
import { readFileSync } from "node:fs";
import type { AddressInfo } from "node:net";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createGrpcTransport } from "@connectrpc/connect-node";
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
class Rt {
  private q: pb.ConnectRequest[] = [];
  private wake?: () => void;
  private done = false;
  private it: AsyncIterator<pb.ConnectResponse>;
  constructor(certName: string, keyName = certName.replace(".pem", ".key"), ca = "ca.pem") {
    const client = createClient(pb.RuntimeGatewayService, createGrpcTransport({
      baseUrl: `https://localhost:${port}`,
      nodeOptions: { cert: cert(certName), key: cert(keyName), ca: cert(ca) },
    }));
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
  async expectNone(ms = 150) {
    const timeout = Symbol();
    const r = await Promise.race([this.pull(), new Promise<symbol>((res) => setTimeout(() => res(timeout), ms).unref())]);
    assert.equal(r, timeout, "unexpected message");
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

test("desired state is pushed after deploy() via the outbox; matching revision gets no message", async () => {
  const { c, r } = await seed("push");
  await repo.deploy({ componentId: c.id, revisionId: r.id, runtimeId: "rt-2" });
  await gw.publishOutbox();
  const rt = new Rt("rt-2.pem");
  rt.hello("rt-2", 1); // already at current revision
  await rt.expectNone();
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
  await eventually(async () => gw.connectedRuntimes().includes("rt-1"));
  const b = new Rt("rt-1.pem");
  b.hello("rt-1", 2);
  await a.next().then(() => assert.fail("expected stream end"), () => {});
  assert.deepEqual(gw.connectedRuntimes(), ["rt-1"]);
  a.close();
  b.close();
  await eventually(async () => gw.connectedRuntimes().length === 0);
  await new Promise((r) => setTimeout(r, 50));
});
