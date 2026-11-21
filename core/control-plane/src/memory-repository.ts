// SPDX-License-Identifier: AGPL-3.0-only
import { randomUUID } from "node:crypto";
import { ApiError, type Component, type DesiredState, type Deployment, type OutboxEntry, type Revision, type Runtime } from "./domain.js";
import type { Repository } from "./repository.js";

const now = () => new Date().toISOString();

/** In-memory Repository used for tests and local dev; mirrors the Postgres semantics. */
export class MemoryRepository implements Repository {
  private components = new Map<string, Component>();
  private revisions: Revision[] = [];
  private runtimes = new Map<string, Runtime>();
  private desired = new Map<string, DesiredState>();
  private deployments = new Map<string, Deployment>();
  private outbox: OutboxEntry[] = [];

  async createComponent(c: { name: string; kind: Component["kind"]; description: string }) {
    for (const x of this.components.values()) if (x.name === c.name) throw new ApiError(409, "conflict", `component ${c.name} exists`);
    const comp: Component = { id: randomUUID(), ...c, createdAt: now() };
    this.components.set(comp.id, comp);
    return comp;
  }
  async getComponent(id: string) { return this.components.get(id); }
  async listComponents() { return [...this.components.values()]; }
  async updateComponent(id: string, patch: { description?: string }) {
    const c = this.components.get(id);
    if (!c) return undefined;
    if (patch.description !== undefined) c.description = patch.description;
    return c;
  }
  async deleteComponent(id: string) {
    if (!this.components.has(id)) return "not_found" as const;
    if (this.revisions.some((r) => r.componentId === id)) return "has_revisions" as const;
    this.components.delete(id);
    return "ok" as const;
  }

  async createRevision(componentId: string, content: unknown, digest: string) {
    if (!this.components.has(componentId)) return undefined;
    const number = this.revisions.filter((r) => r.componentId === componentId).length + 1;
    const rev: Revision = Object.freeze({ id: randomUUID(), componentId, number, digest, content: structuredClone(content), createdAt: now() });
    this.revisions.push(rev);
    return rev;
  }
  async getRevision(componentId: string, number: number) { return this.revisions.find((r) => r.componentId === componentId && r.number === number); }
  async getRevisionById(id: string) { return this.revisions.find((r) => r.id === id); }
  async listRevisions(componentId: string) { return this.revisions.filter((r) => r.componentId === componentId); }

  async upsertRuntime(r: { id: string; name: string; environment: string; version?: string; labels?: Record<string, string> }) {
    const rt: Runtime = { id: r.id, name: r.name, environment: r.environment, status: "online", version: r.version ?? "", labels: r.labels ?? {}, lastSeenAt: now() };
    this.runtimes.set(r.id, rt);
    return rt;
  }
  async getRuntime(id: string) { return this.runtimes.get(id); }
  async listRuntimes(environment?: string) {
    return [...this.runtimes.values()].filter((r) => !environment || r.environment === environment);
  }

  async getDesiredState(runtimeId: string) { return this.desired.get(runtimeId); }
  async deploy(input: { componentId: string; revisionId: string; runtimeId: string }) {
    const rt = this.runtimes.get(input.runtimeId);
    const rev = this.revisions.find((r) => r.id === input.revisionId && r.componentId === input.componentId);
    if (!rt) throw new ApiError(404, "not_found", "runtime not found");
    if (!rev) throw new ApiError(404, "not_found", "revision not found for component");
    const prev = this.desired.get(rt.id);
    const ds: DesiredState = {
      runtimeId: rt.id, environment: rt.environment, generation: (prev?.generation ?? 0) + 1,
      items: { ...(prev?.items ?? {}), [input.componentId]: input.revisionId }, updatedAt: now(),
    };
    this.desired.set(rt.id, ds);
    const dep: Deployment = {
      id: randomUUID(), componentId: input.componentId, revisionId: input.revisionId, runtimeId: rt.id,
      environment: rt.environment, generation: ds.generation, status: "pending", message: "", createdAt: now(), updatedAt: now(),
    };
    this.deployments.set(dep.id, dep);
    this.outbox.push({ id: this.outbox.length + 1, topic: "desired_state.changed", payload: { runtimeId: rt.id, generation: ds.generation }, createdAt: now(), publishedAt: null });
    return { deployment: dep, desiredState: ds };
  }
  async undeploy(input: { componentId: string; runtimeId: string }) {
    const prev = this.desired.get(input.runtimeId);
    if (!prev || !(input.componentId in prev.items)) return undefined;
    const { [input.componentId]: _gone, ...items } = prev.items;
    const ds: DesiredState = { ...prev, generation: prev.generation + 1, items, updatedAt: now() };
    this.desired.set(input.runtimeId, ds);
    this.outbox.push({ id: this.outbox.length + 1, topic: "desired_state.changed", payload: { runtimeId: input.runtimeId, generation: ds.generation }, createdAt: now(), publishedAt: null });
    return ds;
  }
  async getDeployment(id: string) { return this.deployments.get(id); }
  async listDeployments(f: { runtimeId?: string; componentId?: string }) {
    return [...this.deployments.values()].filter((d) => (!f.runtimeId || d.runtimeId === f.runtimeId) && (!f.componentId || d.componentId === f.componentId));
  }
  async updateDeploymentStatus(id: string, status: Deployment["status"], message: string) {
    const d = this.deployments.get(id);
    if (!d) return undefined;
    Object.assign(d, { status, message, updatedAt: now() });
    return d;
  }
  async listOutbox(opts: { unpublishedOnly: boolean }) { return this.outbox.filter((o) => !opts.unpublishedOnly || o.publishedAt === null); }
  async markOutboxPublished(id: number) {
    const o = this.outbox.find((x) => x.id === id);
    if (!o || o.publishedAt !== null) return false;
    o.publishedAt = now();
    return true;
  }
  async touchRuntime(id: string, version?: string) {
    const r = this.runtimes.get(id);
    if (!r) return undefined;
    Object.assign(r, { status: "online" as const, lastSeenAt: now(), ...(version ? { version } : {}) });
    return r;
  }
  async markRuntimeOffline(id: string) { const r = this.runtimes.get(id); if (r) r.status = "offline"; }
}
