// SPDX-License-Identifier: Apache-2.0
// Canonical Nurtuo flow format v1 (draft). Logic and layout are separate: the runtime only
// reads `Flow`; the canvas stores positions in `FlowLayout`. Ids are stable and readable,
// so AI agents and diffs can address steps directly.

export const FLOW_SCHEMA_VERSION = "nurtuo.flow/v1" as const;

export type StepId = string;
export type Port = string; // "default" | "true" | "false" | "try" | "catch" | branch index ...

export type StepKind =
  | "start" | "connector" | "map" | "message" | "setProperties" | "decision" | "branch"
  | "route" | "tryCatch" | "processCall" | "notify" | "stop" | "returnDocuments";

export interface Step {
  id: StepId;
  kind: StepKind;
  label?: string;
  config: Record<string, unknown>;
}

/** Edges are keyed by "source:port" so there is exactly one outgoing edge per port. */
export type Edges = Record<`${StepId}:${Port}`, StepId>;

export interface Flow {
  schema: typeof FLOW_SCHEMA_VERSION;
  id: string;
  name: string;
  revision: number;
  steps: Step[];
  edges: Edges;
}

export interface FlowLayout {
  flowId: string;
  positions: Record<StepId, { x: number; y: number }>;
}

/** CommandBus operations: the canvas and the AI agent issue the same commands. */
export type FlowCommand =
  | { op: "addStep"; step: Step; after?: { step: StepId; port: Port } }
  | { op: "removeStep"; id: StepId }
  | { op: "updateConfig"; id: StepId; patch: Record<string, unknown> }
  | { op: "connect"; from: StepId; port: Port; to: StepId }
  | { op: "disconnect"; from: StepId; port: Port };
