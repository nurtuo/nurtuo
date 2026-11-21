// SPDX-License-Identifier: AGPL-3.0-only
export type ComponentKind = "flow" | "map" | "connector" | "script";
export type DeploymentStatus = "pending" | "running" | "failed" | "stopped";

export interface Component { id: string; name: string; kind: ComponentKind; description: string; createdAt: string }
export interface Revision { id: string; componentId: string; number: number; digest: string; content: unknown; createdAt: string }
export interface Runtime { id: string; name: string; environment: string; status: "online" | "offline"; version: string; labels: Record<string, string>; lastSeenAt: string | null }
export interface DesiredState { runtimeId: string; environment: string; generation: number; items: Record<string, string>; updatedAt: string }
export interface Deployment {
  id: string; componentId: string; revisionId: string; runtimeId: string; environment: string;
  generation: number; status: DeploymentStatus; message: string; createdAt: string; updatedAt: string;
}
export interface OutboxEntry { id: number; topic: string; payload: unknown; createdAt: string; publishedAt: string | null }

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string) { super(message); }
}
