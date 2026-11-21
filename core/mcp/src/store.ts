// SPDX-License-Identifier: AGPL-3.0-only
// Backing store behind the MCP tools. `InMemoryFlowStore` is for tests and the standalone
// `--store memory` mode; `ControlPlaneFlowStore` (control-plane-store.ts) talks to the REST API.
import type { Flow } from "@nurtuo/flow-schema";

export interface ComponentInfo {
  id: string;
  kind: string;
  description: string;
}

export class RevisionConflictError extends Error {
  constructor(readonly flowId: string, readonly expected: number, readonly actual: number) {
    super(`Flow ${flowId} is at revision ${actual}, expected ${expected}`);
    this.name = "RevisionConflictError";
  }
}

export interface FlowStore {
  getFlow(id: string): Promise<Flow | undefined>;
  listFlows(): Promise<Flow[]>;
  listComponents(): Promise<ComponentInfo[]>;
  /** Stores `flow`; `expectedRevision` is the revision the caller read (0 for a new flow). Throws
   *  RevisionConflictError if the stored revision differs. */
  saveFlow(flow: Flow, expectedRevision: number): Promise<void>;
  /** Optional: a specific historical revision (used by `diff_flows`). */
  getFlowRevision?(id: string, revision: number): Promise<Flow | undefined>;
}

export class InMemoryFlowStore implements FlowStore {
  #flows = new Map<string, Flow>();
  #history = new Map<string, Map<number, Flow>>();
  constructor(readonly components: ComponentInfo[] = []) {}

  async getFlow(id: string) {
    const f = this.#flows.get(id);
    return f ? structuredClone(f) : undefined;
  }
  async listFlows() {
    return [...this.#flows.values()].map((f) => structuredClone(f));
  }
  async listComponents() {
    return structuredClone(this.components);
  }
  async saveFlow(flow: Flow, expectedRevision: number) {
    const actual = this.#flows.get(flow.id)?.revision ?? 0;
    if (actual !== expectedRevision) throw new RevisionConflictError(flow.id, expectedRevision, actual);
    this.#flows.set(flow.id, structuredClone(flow));
    const h = this.#history.get(flow.id) ?? new Map<number, Flow>();
    h.set(flow.revision, structuredClone(flow));
    this.#history.set(flow.id, h);
  }
  async getFlowRevision(id: string, revision: number) {
    const f = this.#history.get(id)?.get(revision);
    return f ? structuredClone(f) : undefined;
  }
}

// ---- Deployment (optional capability, only exposed with --allow-deploy / deployment_status) ----

export interface RuntimeInfo {
  id: string;
  name: string;
  environment: string;
  status?: string;
}

export interface DeploymentInfo {
  id: string;
  flowId: string;
  /** Flow revision number that was deployed. */
  revision: number;
  runtimeId: string;
  status: string;
  message?: string;
  generation?: number;
  createdAt?: string;
  updatedAt?: string;
}

export interface Deployer {
  listRuntimes(): Promise<RuntimeInfo[]>;
  /** Requests a deployment of `flowId` at `revision` (default: latest) to a runtime. */
  deploy(req: { flowId: string; runtimeId: string; revision?: number }): Promise<DeploymentInfo>;
  getDeployment(id: string): Promise<DeploymentInfo | undefined>;
  listDeployments(filter?: { flowId?: string; runtimeId?: string }): Promise<DeploymentInfo[]>;
}

/** Test/standalone deployer: records deployments as `pending`; nothing ever runs them. */
export class InMemoryDeployer implements Deployer {
  #deployments = new Map<string, DeploymentInfo>();
  #n = 0;
  constructor(private readonly store: FlowStore, readonly runtimes: RuntimeInfo[] = [{ id: "local", name: "local", environment: "dev", status: "online" }]) {}

  async listRuntimes() {
    return structuredClone(this.runtimes);
  }
  async deploy(req: { flowId: string; runtimeId: string; revision?: number }) {
    const flow = await this.store.getFlow(req.flowId);
    if (!flow) throw new Error(`Flow ${req.flowId} not found`);
    if (!this.runtimes.some((r) => r.id === req.runtimeId)) throw new Error(`Runtime ${req.runtimeId} not found`);
    const now = new Date().toISOString();
    const d: DeploymentInfo = {
      id: `dep-${++this.#n}`, flowId: req.flowId, revision: req.revision ?? flow.revision,
      runtimeId: req.runtimeId, status: "pending", generation: this.#n, createdAt: now, updatedAt: now,
    };
    this.#deployments.set(d.id, d);
    return structuredClone(d);
  }
  async getDeployment(id: string) {
    const d = this.#deployments.get(id);
    return d ? structuredClone(d) : undefined;
  }
  async listDeployments(filter: { flowId?: string; runtimeId?: string } = {}) {
    return [...this.#deployments.values()]
      .filter((d) => (!filter.flowId || d.flowId === filter.flowId) && (!filter.runtimeId || d.runtimeId === filter.runtimeId))
      .map((d) => structuredClone(d));
  }
  /** Test helper: simulate the runtime reporting back. */
  setStatus(id: string, status: string, message?: string) {
    const d = this.#deployments.get(id);
    if (d) Object.assign(d, { status, ...(message ? { message } : {}), updatedAt: new Date().toISOString() });
  }
}
