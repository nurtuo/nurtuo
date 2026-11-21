// SPDX-License-Identifier: AGPL-3.0-only
import type { Component, ComponentKind, DesiredState, Deployment, DeploymentStatus, OutboxEntry, Revision, Runtime } from "./domain.js";

export interface DeployInput { componentId: string; revisionId: string; runtimeId: string }

export interface Repository {
  createComponent(c: { name: string; kind: ComponentKind; description: string }): Promise<Component>;
  getComponent(id: string): Promise<Component | undefined>;
  listComponents(): Promise<Component[]>;
  updateComponent(id: string, patch: { description?: string }): Promise<Component | undefined>;
  /** Deleting a component that has revisions is refused ("has_revisions"). */
  deleteComponent(id: string): Promise<"ok" | "not_found" | "has_revisions">;

  /** Assigns the next revision number atomically; content is stored immutably. */
  createRevision(componentId: string, content: unknown, digest: string): Promise<Revision | undefined>;
  getRevision(componentId: string, number: number): Promise<Revision | undefined>;
  getRevisionById(id: string): Promise<Revision | undefined>;
  listRevisions(componentId: string): Promise<Revision[]>;

  upsertRuntime(r: { id: string; name: string; environment: string; version?: string; labels?: Record<string, string> }): Promise<Runtime>;
  getRuntime(id: string): Promise<Runtime | undefined>;
  listRuntimes(environment?: string): Promise<Runtime[]>;

  getDesiredState(runtimeId: string): Promise<DesiredState | undefined>;
  /** One transaction: desired state generation+1, deployment row, outbox row. */
  deploy(input: DeployInput): Promise<{ deployment: Deployment; desiredState: DesiredState }>;
  /** Removes a component from the runtime's desired state (generation+1, outbox row). Undefined when it was not deployed there. */
  undeploy(input: { componentId: string; runtimeId: string }): Promise<DesiredState | undefined>;
  getDeployment(id: string): Promise<Deployment | undefined>;
  listDeployments(filter: { runtimeId?: string; componentId?: string }): Promise<Deployment[]>;
  updateDeploymentStatus(id: string, status: DeploymentStatus, message: string): Promise<Deployment | undefined>;

  listOutbox(opts: { unpublishedOnly: boolean }): Promise<OutboxEntry[]>;
  /** Marks an outbox row published; returns false when it does not exist or was already published. */
  markOutboxPublished(id: number): Promise<boolean>;

  /** Gateway liveness: bumps last_seen_at (status online) for a registered runtime; optionally records its version. */
  touchRuntime(id: string, version?: string): Promise<Runtime | undefined>;
  markRuntimeOffline(id: string): Promise<void>;
}
