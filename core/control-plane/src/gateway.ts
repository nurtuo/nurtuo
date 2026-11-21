// SPDX-License-Identifier: AGPL-3.0-only
// Runtime gateway server (Connect-ES, gRPC protocol over HTTP/2 + mTLS). Runtimes dial out; the
// control plane never connects to a runtime. Identity comes from the client certificate.
import { createSecureServer, type Http2SecureServer } from "node:http2";
import type { TLSSocket } from "node:tls";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createContextKey, createContextValues, type ConnectRouter, type HandlerContext } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { runtimev1 as pb } from "@nurtuo/proto";
import type { DeploymentStatus } from "./domain.js";
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

export interface GatewayOptions {
  /** A session with no message for this long is closed (default 0 = disabled). */
  idleTimeoutMs?: number;
  onCommandResult?: (runtimeId: string, r: pb.CommandResult) => void;
  log?: (msg: string) => void;
}

export class Gateway {
  private sessions = new Map<string, Session>();
  private pendingCommands = new Map<string, (r: pb.CommandResult) => void>();
  private publishing: Promise<number> | undefined;
  private timer: NodeJS.Timeout | undefined;

  constructor(private repo: Repository, private opts: GatewayOptions = {}) {}

  connectedRuntimes(): string[] { return [...this.sessions.keys()]; }

  /** Current desired state of a runtime as a wire message; undefined if it never had one. */
  async desiredStateMessage(runtimeId: string): Promise<pb.DesiredState | undefined> {
    const ds = await this.repo.getDesiredState(runtimeId);
    if (!ds) return undefined;
    const deployments: pb.Deployment[] = [];
    for (const [componentId, revisionId] of Object.entries(ds.items).sort(([a], [b]) => a.localeCompare(b))) {
      const rev = await this.repo.getRevisionById(revisionId);
      if (!rev) continue;
      // deployment_id is the component id: one slot per component per runtime.
      deployments.push(create(pb.DeploymentSchema, { deploymentId: componentId, bundleDigest: rev.digest }));
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
    });
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
          if (ds && ds.revision !== m.value.desiredStateRevision) {
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
