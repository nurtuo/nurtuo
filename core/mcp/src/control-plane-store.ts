// SPDX-License-Identifier: AGPL-3.0-only
// FlowStore + Deployer backed by the control-plane REST API.
//
// Mapping: a flow is a control-plane component of kind `flow` whose *name* is the flow id. Every
// saved flow is an immutable revision of that component; the server assigns revision numbers
// 1, 2, 3, ... so `flow.revision` equals the control-plane revision number. Optimistic concurrency:
// `saveFlow(flow, expected)` compares `expected` with the latest revision number before writing
// and re-checks the number the server assigned afterwards (a concurrent writer that slipped in
// between yields RevisionConflictError; its revision stays in history, nothing is lost).
import { FLOW_SCHEMA_VERSION, type Flow } from "@nurtuo/flow-schema";
import { ControlPlaneClient, ControlPlaneError, type ControlPlaneClientOptions } from "./control-plane-client.js";
import { ToolError } from "./errors.js";
import {
  RevisionConflictError, type ComponentInfo, type DeploymentInfo, type Deployer, type FlowStore, type RuntimeInfo,
} from "./store.js";

interface CpComponent { id: string; name: string; kind: string; description: string }
interface CpRevision { id: string; componentId: string; number: number; content: unknown }
interface CpDeployment {
  id: string; componentId: string; revisionId: string; runtimeId: string; generation: number;
  status: string; message?: string; createdAt?: string; updatedAt?: string;
}

const NAME_PATTERN = /^[a-z0-9][a-z0-9._-]*$/;

/** Converts control-plane failures into structured tool errors. */
export function mapControlPlaneError(e: unknown): never {
  if (e instanceof ToolError || e instanceof RevisionConflictError) throw e;
  if (e instanceof ControlPlaneError) {
    if (e.status === 401 || e.status === 403) throw new ToolError("unauthorized", `control plane rejected the credentials (HTTP ${e.status})`);
    if (e.status === 404) throw new ToolError("not_found", e.message);
    if (e.status === 400) throw new ToolError("invalid_input", e.message);
    if (e.code === "timeout") throw new ToolError("timeout", e.message);
    throw new ToolError("backend_error", e.message, { status: e.status, backendCode: e.code });
  }
  throw e;
}

export class ControlPlaneFlowStore implements FlowStore, Deployer {
  readonly #api: ControlPlaneClient;
  /** flow id (component name) -> component id. Only positive entries are cached. */
  readonly #ids = new Map<string, string>();

  constructor(opts: ControlPlaneClientOptions | ControlPlaneClient) {
    this.#api = opts instanceof ControlPlaneClient ? opts : new ControlPlaneClient(opts);
  }

  async #components(): Promise<CpComponent[]> {
    try {
      const r = await this.#api.request<{ items: CpComponent[] }>("GET", "/v1/components");
      for (const c of r.items) if (c.kind === "flow") this.#ids.set(c.name, c.id);
      return r.items;
    } catch (e) { return mapControlPlaneError(e); }
  }

  async #componentId(flowId: string): Promise<string | undefined> {
    const cached = this.#ids.get(flowId);
    if (cached) return cached;
    const found = (await this.#components()).find((c) => c.kind === "flow" && c.name === flowId);
    return found?.id;
  }

  async #revisions(componentId: string): Promise<CpRevision[]> {
    try {
      const r = await this.#api.request<{ items: CpRevision[] }>("GET", `/v1/components/${encodeURIComponent(componentId)}/revisions`);
      return r.items;
    } catch (e) {
      if (e instanceof ControlPlaneError && e.status === 404) {
        for (const [k, v] of this.#ids) if (v === componentId) this.#ids.delete(k); // stale cache
        return [];
      }
      return mapControlPlaneError(e);
    }
  }

  static #toFlow(rev: CpRevision): Flow {
    const c = rev.content as Flow;
    return { ...c, schema: c.schema ?? FLOW_SCHEMA_VERSION, revision: rev.number };
  }

  async getFlow(id: string): Promise<Flow | undefined> {
    const cid = await this.#componentId(id);
    if (!cid) return undefined;
    const revs = await this.#revisions(cid);
    const latest = revs.reduce<CpRevision | undefined>((a, r) => (!a || r.number > a.number ? r : a), undefined);
    return latest ? ControlPlaneFlowStore.#toFlow(latest) : undefined;
  }

  async getFlowRevision(id: string, revision: number): Promise<Flow | undefined> {
    const cid = await this.#componentId(id);
    if (!cid) return undefined;
    try {
      return ControlPlaneFlowStore.#toFlow(await this.#api.request<CpRevision>("GET", `/v1/components/${encodeURIComponent(cid)}/revisions/${revision}`));
    } catch (e) {
      if (e instanceof ControlPlaneError && e.status === 404) return undefined;
      return mapControlPlaneError(e);
    }
  }

  async listFlows(): Promise<Flow[]> {
    const flows = (await this.#components()).filter((c) => c.kind === "flow");
    const out: Flow[] = [];
    // Small batches keep us polite towards the control plane.
    for (let i = 0; i < flows.length; i += 8) {
      const batch = await Promise.all(flows.slice(i, i + 8).map((c) => this.getFlow(c.name)));
      for (const f of batch) if (f) out.push(f);
    }
    return out;
  }

  async listComponents(): Promise<ComponentInfo[]> {
    return (await this.#components())
      .filter((c) => c.kind !== "flow")
      .map((c) => ({ id: c.name, kind: c.kind, description: c.description }));
  }

  async saveFlow(flow: Flow, expectedRevision: number): Promise<void> {
    if (!NAME_PATTERN.test(flow.id)) {
      throw new ToolError("invalid_input", `Flow id ${JSON.stringify(flow.id)} cannot be stored in the control plane: ids must match ${NAME_PATTERN.source}`);
    }
    let cid = await this.#componentId(flow.id);
    let latest = 0;
    if (cid) latest = (await this.#revisions(cid)).reduce((m, r) => Math.max(m, r.number), 0);
    if (latest !== expectedRevision) throw new RevisionConflictError(flow.id, expectedRevision, latest);
    try {
      if (!cid) {
        try {
          cid = (await this.#api.request<CpComponent>("POST", "/v1/components", {
            body: { name: flow.id, kind: "flow", description: flow.name },
          })).id;
          this.#ids.set(flow.id, cid);
        } catch (e) {
          // Someone created it between our lookup and the POST.
          if (e instanceof ControlPlaneError && e.status === 409) throw new RevisionConflictError(flow.id, expectedRevision, Math.max(1, expectedRevision));
          throw e;
        }
      }
      const created = await this.#api.request<CpRevision>("POST", `/v1/components/${encodeURIComponent(cid)}/revisions`, { body: { content: flow } });
      if (created.number !== expectedRevision + 1) throw new RevisionConflictError(flow.id, expectedRevision, created.number - 1);
    } catch (e) { mapControlPlaneError(e); }
  }

  // ---- Deployer ----

  async listRuntimes(): Promise<RuntimeInfo[]> {
    try {
      const r = await this.#api.request<{ items: RuntimeInfo[] }>("GET", "/v1/runtimes");
      return r.items.map((x) => ({ id: x.id, name: x.name, environment: x.environment, ...(x.status ? { status: x.status } : {}) }));
    } catch (e) { return mapControlPlaneError(e); }
  }

  async deploy(req: { flowId: string; runtimeId: string; revision?: number }): Promise<DeploymentInfo> {
    const cid = await this.#componentId(req.flowId);
    if (!cid) throw new ToolError("not_found", `Flow ${req.flowId} not found`);
    try {
      const revision = req.revision ?? (await this.#revisions(cid)).reduce((m, r) => Math.max(m, r.number), 0);
      if (revision < 1) throw new ToolError("not_found", `Flow ${req.flowId} has no revisions`);
      const r = await this.#api.request<{ deployment: CpDeployment }>("POST", "/v1/deployments", {
        body: { componentId: cid, runtimeId: req.runtimeId, revision },
      });
      return this.#toDeployment(r.deployment, req.flowId, revision);
    } catch (e) { return mapControlPlaneError(e); }
  }

  async getDeployment(id: string): Promise<DeploymentInfo | undefined> {
    try {
      return await this.#hydrate(await this.#api.request<CpDeployment>("GET", `/v1/deployments/${encodeURIComponent(id)}`));
    } catch (e) {
      if (e instanceof ControlPlaneError && e.status === 404) return undefined;
      return mapControlPlaneError(e);
    }
  }

  async listDeployments(filter: { flowId?: string; runtimeId?: string } = {}): Promise<DeploymentInfo[]> {
    let componentId: string | undefined;
    if (filter.flowId) {
      componentId = await this.#componentId(filter.flowId);
      if (!componentId) return [];
    }
    try {
      const r = await this.#api.request<{ items: CpDeployment[] }>("GET", "/v1/deployments", { query: { runtimeId: filter.runtimeId, componentId } });
      return Promise.all(r.items.map((d) => this.#hydrate(d)));
    } catch (e) { return mapControlPlaneError(e); }
  }

  /** Resolves flow id and revision number of a deployment from its component / revision ids. */
  async #hydrate(d: CpDeployment): Promise<DeploymentInfo> {
    let flowId = [...this.#ids].find(([, v]) => v === d.componentId)?.[0];
    if (!flowId) {
      await this.#components();
      flowId = [...this.#ids].find(([, v]) => v === d.componentId)?.[0];
    }
    const revs = await this.#revisions(d.componentId);
    const revision = revs.find((r) => r.id === d.revisionId)?.number ?? 0;
    return this.#toDeployment(d, flowId ?? d.componentId, revision);
  }

  #toDeployment(d: CpDeployment, flowId: string, revision: number): DeploymentInfo {
    return {
      id: d.id, flowId, revision, runtimeId: d.runtimeId, status: d.status, generation: d.generation,
      ...(d.message ? { message: d.message } : {}),
      ...(d.createdAt ? { createdAt: d.createdAt } : {}), ...(d.updatedAt ? { updatedAt: d.updatedAt } : {}),
    };
  }
}
