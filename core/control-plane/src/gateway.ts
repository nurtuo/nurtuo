// SPDX-License-Identifier: AGPL-3.0-only
// Runtime gateway server (Connect-ES, gRPC protocol over HTTP/2 + mTLS). Runtimes dial out; the
// control plane never connects to a runtime. Identity comes from the client certificate.
import { createHash } from "node:crypto";
import { createSecureServer, type Http2SecureServer } from "node:http2";
import type { TLSSocket } from "node:tls";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createContextKey, createContextValues, type ConnectRouter, type HandlerContext } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { runtimev1 as pb } from "@nurtuo/proto";
import { digestOf } from "./api.js";
import { bundleNamePattern, maxBundleSize, packBundle, type SigningKey } from "./bundle.js";
import type { DeploymentStatus, Revision } from "./domain.js";
import type { Repository } from "./repository.js";

/** Runtime id carried in a certificate: URI SAN spiffe://nurtuo/runtime/<id> wins, else the subject CN. */
export function runtimeIdFromCert(cert: { subject?: { CN?: string | string[] }; subjectaltname?: string } | undefined): string | undefined {
  if (!cert) return undefined;
  for (const part of (cert.subjectaltname ?? "").split(/,\s*/)) {
    const m = /^URI:spiffe:\/\/nurtuo\/runtime\/(.+)$/.exec(part);
    if (m?.[1]) return m[1];
  }
  const cn = cert.subject?.CN;
  const v = Array.isArray(cn) ? cn[0] : cn;
  return v || undefined;
}

const peerKey = createContextKey<string | undefined>(undefined, { description: "runtime id from mTLS client cert" });
const artifactChunkSize = 1024 * 1024;
const maxArtifactStreams = 8;

/** runtime-reported state (open set) -> persisted deployment status. Unknown states are ignored. */
const STATE_MAP: Record<string, DeploymentStatus> = { applying: "pending", running: "running", paused: "stopped", error: "failed", stopped: "stopped" };

class Queue<T> {
  private items: T[] = [];
  private waiter: (() => void) | undefined;
  private closed = false;
  private error: unknown;
  push(v: T) { if (!this.closed) { this.items.push(v); this.wake(); } }
  close(err?: unknown) { if (!this.closed) { this.closed = true; this.error = err; this.wake(); } }
  private wake() { const w = this.waiter; this.waiter = undefined; w?.(); }
  async *drain(): AsyncGenerator<T> {
    for (;;) {
      while (this.items.length) yield this.items.shift()!;
      if (this.closed) { if (this.error) throw this.error; return; }
      await new Promise<void>((r) => { this.waiter = r; });
    }
  }
}

interface Session { runtimeId: string; out: Queue<pb.ConnectResponse>; lastSeen: number }
interface Artifact { data: Buffer; digest: string; references: number }
class BundlePackError extends Error {}

interface ArtifactReference {
  componentId: string;
  revisionId: string;
  packed: boolean;
  packError?: string;
}

export interface GatewayOptions {
  /** A session with no message for this long is closed (default 0 = disabled). */
  idleTimeoutMs?: number;
  onCommandResult?: (runtimeId: string, r: pb.CommandResult) => void;
  log?: (msg: string) => void;
  signingKey?: SigningKey;
  artifactCacheBytes?: number;
}

export class Gateway {
  private sessions = new Map<string, Session>();
  private pendingCommands = new Map<string, (r: pb.CommandResult) => void>();
  private publishing: Promise<number> | undefined;
  private timer: NodeJS.Timeout | undefined;
  private artifacts = new Map<string, string>(); // revision and key id -> archive digest
  private artifactsByDigest = new Map<string, Artifact>();
  private artifactBytes = 0;
  private packing = new Map<string, Promise<Artifact | undefined>>();
  private artifactViews = new Map<string, { generation: number; byDigest: Map<string, ArtifactReference[]> }>();
  private activeArtifactStreams = 0;

  constructor(private repo: Repository, private opts: GatewayOptions = {}) {}

  connectedRuntimes(): string[] { return [...this.sessions.keys()]; }

  private async artifactFor(rev: Revision): Promise<Artifact | undefined> {
    const key = this.opts.signingKey;
    if (!key) return undefined;
    const cacheKey = `${rev.id}:${key.keyId}`;
    const cached = this.artifacts.get(cacheKey);
    if (cached) return this.artifactsByDigest.get(cached)!;
    const pending = this.packing.get(cacheKey);
    if (pending) return pending;
    const packing = this.packArtifact(rev, key, cacheKey).finally(() => this.packing.delete(cacheKey));
    this.packing.set(cacheKey, packing);
    return packing;
  }

  private async packArtifact(rev: Revision, key: SigningKey, cacheKey: string): Promise<Artifact | undefined> {
    if (rev.digest !== digestOf(rev.content)) return undefined;
    const component = await this.repo.getComponent(rev.componentId);
    if (!component) throw new BundlePackError("bundle: revision component is missing");
    let data: Buffer;
    try {
      data = packBundle({
        name: bundleNamePattern.test(component.name) ? component.name : component.id,
        createdAt: rev.createdAt,
        component: { kind: component.kind, id: component.id, revision: rev.number, document: rev.content },
      }, key);
    } catch (error) {
      throw new BundlePackError(error instanceof Error ? error.message : "bundle packing failed");
    }
    const digest = `sha256:${createHash("sha256").update(data).digest("hex")}`;
    const capacity = this.opts.artifactCacheBytes ?? maxBundleSize;
    if (data.length > capacity) return { data, digest, references: 0 };
    while (!this.artifactsByDigest.has(digest) && this.artifactBytes + data.length > capacity) {
      const oldest = this.artifacts.entries().next().value!;
      this.artifacts.delete(oldest[0]);
      const evicted = this.artifactsByDigest.get(oldest[1])!;
      if (--evicted.references === 0) {
        this.artifactsByDigest.delete(evicted.digest);
        this.artifactBytes -= evicted.data.length;
      }
    }
    let artifact = this.artifactsByDigest.get(digest);
    if (!artifact) {
      artifact = { data, digest, references: 0 };
      this.artifactsByDigest.set(digest, artifact);
      this.artifactBytes += data.length;
    }
    artifact.references++;
    this.artifacts.set(cacheKey, digest);
    return artifact;
  }

  private async resolveArtifact(rev: Revision) {
    try {
      const artifact = await this.artifactFor(rev);
      return { digest: artifact?.digest ?? rev.digest, artifact };
    } catch (error) {
      this.opts.log?.(`cannot pack revision ${rev.id}`);
      const packError = error instanceof BundlePackError ? error.message : "bundle packing failed";
      return { digest: rev.digest, packError };
    }
  }

  /** Current desired state of a runtime as a wire message; undefined if it never had one. */
  async desiredStateMessage(runtimeId: string): Promise<pb.DesiredState | undefined> {
    const ds = await this.repo.getDesiredState(runtimeId);
    if (!ds) return undefined;
    const deployments: pb.Deployment[] = [];
    const byDigest = new Map<string, ArtifactReference[]>();
    for (const [componentId, revisionId] of Object.entries(ds.items).sort(([a], [b]) => a.localeCompare(b))) {
      const rev = await this.repo.getRevisionById(revisionId);
      if (!rev) continue;
      // deployment_id is the component id: one slot per component per runtime.
      const { digest: bundleDigest, artifact, packError } = await this.resolveArtifact(rev);
      if (this.opts.signingKey) {
        const reference = { componentId, revisionId, packed: artifact !== undefined, packError };
        const references = byDigest.get(bundleDigest);
        if (references) references.push(reference);
        else byDigest.set(bundleDigest, [reference]);
      }
      deployments.push(create(pb.DeploymentSchema, { deploymentId: componentId, bundleDigest }));
    }
    if (this.opts.signingKey && (this.artifactViews.get(runtimeId)?.generation ?? -1) <= ds.generation) {
      this.artifactViews.set(runtimeId, { generation: ds.generation, byDigest });
    }
    return create(pb.DesiredStateSchema, { revision: BigInt(ds.generation), deployments });
  }

  /** Sends the runtime's current desired state if it is connected. */
  async pushDesiredState(runtimeId: string): Promise<boolean> {
    const s = this.sessions.get(runtimeId);
    if (!s) return false;
    const msg = await this.desiredStateMessage(runtimeId);
    if (!msg) return false;
    s.out.push(create(pb.ConnectResponseSchema, { msg: { case: "desiredState", value: msg } }));
    return true;
  }

  /** Sends a command to a connected runtime and resolves with its result. */
  sendCommand(runtimeId: string, name: string, args: Record<string, string> = {}, timeoutMs = 10_000): Promise<pb.CommandResult> {
    const s = this.sessions.get(runtimeId);
    if (!s) return Promise.reject(new Error(`runtime ${runtimeId} not connected`));
    const commandId = crypto.randomUUID();
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => { this.pendingCommands.delete(commandId); reject(new Error("command timed out")); }, timeoutMs);
      this.pendingCommands.set(commandId, (r) => { clearTimeout(t); resolve(r); });
      s.out.push(create(pb.ConnectResponseSchema, { msg: { case: "command", value: create(pb.CommandSchema, { commandId, name, args }) } }));
    });
  }

  /**
   * Outbox publisher: pushes DesiredState for every unpublished desired_state.changed row to the
   * connected runtime and marks the row published. Rows for runtimes that are offline are marked
   * published too: the Hello reconcile delivers current state on reconnect.
   */
  publishOutbox(): Promise<number> {
    // Serialise: concurrent callers share one in-flight run so a row is never pushed twice.
    this.publishing ??= this.runPublish().finally(() => { this.publishing = undefined; });
    return this.publishing;
  }
  private async runPublish(): Promise<number> {
    let n = 0;
    for (const row of await this.repo.listOutbox({ unpublishedOnly: true })) {
      if (row.topic === "desired_state.changed") {
        const runtimeId = (row.payload as { runtimeId?: string } | null)?.runtimeId;
        if (runtimeId) await this.pushDesiredState(runtimeId);
      }
      if (await this.repo.markOutboxPublished(row.id)) n++;
    }
    return n;
  }

  startOutboxPublisher(intervalMs = 500) {
    this.stopOutboxPublisher();
    this.timer = setInterval(() => { this.publishOutbox().catch((e) => this.opts.log?.(`outbox publish failed: ${e}`)); }, intervalMs);
    this.timer.unref();
  }
  stopOutboxPublisher() { if (this.timer) clearInterval(this.timer); this.timer = undefined; }

  /** Closes all sessions (used on shutdown). */
  closeAll() { for (const s of this.sessions.values()) s.out.close(); }

  register(router: ConnectRouter) {
    router.service(pb.RuntimeGatewayService, {
      connect: (reqs, ctx) => this.connect(reqs, ctx),
      fetchArtifact: (req, ctx) => this.fetchArtifact(req, ctx),
    });
  }

  private async *fetchArtifact(req: pb.FetchArtifactRequest, ctx: HandlerContext): AsyncGenerator<pb.FetchArtifactResponse> {
    const certId = ctx.values.get(peerKey);
    if (!certId) throw new ConnectError("client certificate carries no runtime identity", Code.Unauthenticated);
    const hex = req.digest.replace(/^sha256:/, "");
    if (hex.length !== 64 || !/^[0-9a-f]+$/.test(hex)) {
      throw new ConnectError("expected a SHA-256 digest", Code.InvalidArgument);
    }
    if (!this.opts.signingKey) {
      throw new ConnectError("control plane has no bundle signing key configured (NURTUO_BUNDLE_SIGNING_KEY_FILE)", Code.FailedPrecondition);
    }
    const digest = `sha256:${hex}`;
    const notFound = new ConnectError("artifact not found for this runtime", Code.NotFound);
    const references = this.artifactViews.get(certId)?.byDigest.get(digest);
    if (!references) throw notFound;
    const state = await this.repo.getDesiredState(certId);
    const current = references.filter(({ componentId, revisionId }) => state?.items[componentId] === revisionId);
    const reference = current.find(({ packed }) => packed);
    if (!reference) {
      const packError = current.find((entry) => entry.packError)?.packError;
      if (packError) throw new ConnectError(packError, Code.FailedPrecondition);
      if (current.length) {
        throw new ConnectError("artifact is not stored by the control plane; place it in bundle_dir", Code.NotFound);
      }
      throw notFound;
    }
    if (ctx.signal.aborted) return;
    if (this.activeArtifactStreams >= maxArtifactStreams) {
      throw new ConnectError("too many active artifact streams", Code.ResourceExhausted);
    }
    this.activeArtifactStreams++;
    try {
      let artifact = this.artifactsByDigest.get(digest);
      if (!artifact) {
        const rev = await this.repo.getRevisionById(reference.revisionId);
        if (!rev) throw notFound;
        const resolved = await this.resolveArtifact(rev);
        if (resolved.packError) throw new ConnectError(resolved.packError, Code.FailedPrecondition);
        artifact = resolved.artifact;
      }
      if (!artifact || artifact.digest !== digest) throw notFound;
      // Reject an artifact that was superseded while its archive was being rebuilt.
      const latest = await this.repo.getDesiredState(certId);
      if (latest?.items[reference.componentId] !== reference.revisionId) throw notFound;
      for (let offset = 0; offset < artifact.data.length && !ctx.signal.aborted; offset += artifactChunkSize) {
        yield create(pb.FetchArtifactResponseSchema, { data: artifact.data.subarray(offset, offset + artifactChunkSize) });
      }
    } finally {
      this.activeArtifactStreams--;
    }
  }

  private async *connect(reqs: AsyncIterable<pb.ConnectRequest>, ctx: HandlerContext): AsyncGenerator<pb.ConnectResponse> {
    const certId = ctx.values.get(peerKey);
    if (!certId) throw new ConnectError("client certificate carries no runtime identity", Code.Unauthenticated);
    const out = new Queue<pb.ConnectResponse>();
    const session: Session = { runtimeId: certId, out, lastSeen: Date.now() };
    let registered = false;

    const idle = this.opts.idleTimeoutMs ? setInterval(() => {
      if (Date.now() - session.lastSeen > this.opts.idleTimeoutMs!) out.close(new ConnectError("idle timeout", Code.DeadlineExceeded));
    }, Math.max(50, this.opts.idleTimeoutMs / 4)) : undefined;
    idle?.unref();

    const consume = async () => {
      let first = true;
      for await (const req of reqs) {
        session.lastSeen = Date.now();
        const m = req.msg;
        if (first) {
          first = false;
          if (m.case !== "hello") throw new ConnectError("first message must be Hello", Code.InvalidArgument);
          if (m.value.runtimeId !== certId) {
            throw new ConnectError(`hello runtime_id ${JSON.stringify(m.value.runtimeId)} does not match certificate identity`, Code.PermissionDenied);
          }
          if (!(await this.repo.touchRuntime(certId, m.value.version))) throw new ConnectError(`runtime ${certId} is not registered`, Code.PermissionDenied);
          const prev = this.sessions.get(certId);
          this.sessions.set(certId, session);
          registered = true;
          prev?.out.close(); // newest connection wins
          const ds = await this.desiredStateMessage(certId);
          // Signing configuration can change the payload at the same stored generation.
          if (ds) {
            out.push(create(pb.ConnectResponseSchema, { msg: { case: "desiredState", value: ds } }));
          }
          continue;
        }
        switch (m.case) {
          case "hello": throw new ConnectError("duplicate Hello", Code.InvalidArgument);
          case "heartbeat": await this.repo.touchRuntime(certId); break;
          case "deploymentStatus": await this.applyStatus(certId, m.value); break;
          case "commandResult": {
            this.pendingCommands.get(m.value.commandId)?.(m.value);
            this.pendingCommands.delete(m.value.commandId);
            this.opts.onCommandResult?.(certId, m.value);
            break;
          }
        }
      }
      out.close(); // client half-closed
    };
    consume().catch((e) => out.close(e instanceof ConnectError ? e : new ConnectError(String(e), Code.Internal)));

    try {
      yield* out.drain();
    } finally {
      if (idle) clearInterval(idle);
      if (registered && this.sessions.get(certId) === session) {
        this.sessions.delete(certId);
        await this.repo.markRuntimeOffline(certId).catch(() => {});
      }
    }
  }

  private async applyStatus(runtimeId: string, s: pb.DeploymentStatus) {
    const status = STATE_MAP[s.state];
    if (!status) { this.opts.log?.(`ignoring unknown deployment state ${s.state}`); return; }
    // deployment_id is the component id; revision is the desired-state generation being applied.
    // The runtime re-reports a component at the *current* generation when it re-applies it (retry
    // of a failed deployment, a sibling's change), so match the newest deployment row at or below it.
    const deps = await this.repo.listDeployments({ runtimeId, componentId: s.deploymentId });
    const d = deps.filter((x) => x.generation <= Number(s.revision)).sort((a, b) => b.generation - a.generation)[0];
    if (!d) { this.opts.log?.(`no deployment for ${s.deploymentId}@${s.revision} on ${runtimeId}`); return; }
    await this.repo.updateDeploymentStatus(d.id, status, s.message);
  }
}

export interface GatewayServerOptions { cert: string | Buffer; key: string | Buffer; ca: string | Buffer }

/** HTTP/2 TLS 1.3 server requiring a client certificate signed by `ca`. */
export function createGatewayServer(gw: Gateway, o: GatewayServerOptions): Http2SecureServer {
  const handler = connectNodeAdapter({
    routes: (r) => gw.register(r),
    contextValues: (req) => {
      const sock = req.socket as TLSSocket;
      const id = sock.authorized ? runtimeIdFromCert(sock.getPeerCertificate()) : undefined;
      return createContextValues().set(peerKey, id);
    },
  });
  return createSecureServer({ cert: o.cert, key: o.key, ca: o.ca, requestCert: true, rejectUnauthorized: true, minVersion: "TLSv1.3" }, handler);
}
