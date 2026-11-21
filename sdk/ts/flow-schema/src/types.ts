// SPDX-License-Identifier: Apache-2.0
// Canonical Nurtuo flow format v1. Logic and layout are separate: the runtime only reads
// `Flow`; the canvas stores positions in `FlowLayout`. Ids are stable and readable, so AI
// agents and diffs can address steps directly.
//
// schema/flow.v1.schema.json is the source of truth; these types are kept consistent with it by
// schema.test.ts (kind list, version) and by the shared conformance corpus.

export const FLOW_SCHEMA_VERSION = "nurtuo.flow/v1" as const;

export const STEP_KINDS = [
  "start", "connector", "queue", "map", "script", "message", "setProperties", "decision", "branch",
  "route", "tryCatch", "processCall", "notify", "stop", "returnDocuments",
] as const;

export type StepId = string;
/** "default" | "true" | "false" | "try" | "catch" | "1".."25" (branch) | route id */
export type Port = string;
export type StepKind = (typeof STEP_KINDS)[number];

export interface StartConfig {
  type: "manual" | "listener" | "schedule" | "queue";
  connector?: string;
  path?: string;
  method?: string;
  queue?: string;
  cron?: string;
}
export interface ConnectorConfig { connector: string; operation: string; objectType?: string; connection?: string }
export interface QueueConfig { queue: string; operation: "publish" }
export interface MapConfig { jsonata: string }
export interface ScriptConfig { script: string; timeoutMs?: number; memoryMiB?: number; capabilities?: ("clock" | "random")[] }
export interface MessageConfig { template: string }
export interface PropertyAssignment { name: string; scope?: "document" | "process"; value: string }
export interface SetPropertiesConfig { properties: PropertyAssignment[] }
export interface DecisionConfig { cel: string }
export interface BranchConfig { paths: number }
export interface RouteConfig { routes: { id: string; cel: string }[] }
export interface TryCatchConfig { retries?: number }
export interface ProcessCallConfig { flow: string; revision?: number }
export interface NotifyConfig { channel: string; template?: string }
export interface StopConfig { status?: number; message?: string }
export type ReturnDocumentsConfig = Record<string, never>;

export interface StepConfigByKind {
  start: StartConfig;
  connector: ConnectorConfig;
  queue: QueueConfig;
  map: MapConfig;
  script: ScriptConfig;
  message: MessageConfig;
  setProperties: SetPropertiesConfig;
  decision: DecisionConfig;
  branch: BranchConfig;
  route: RouteConfig;
  tryCatch: TryCatchConfig;
  processCall: ProcessCallConfig;
  notify: NotifyConfig;
  stop: StopConfig;
  returnDocuments: ReturnDocumentsConfig;
}

/** A step of kind K with its typed config. */
export type TypedStep<K extends StepKind = StepKind> = {
  [P in K]: { id: StepId; kind: P; label?: string; config: StepConfigByKind[P] };
}[K];

/** Loose step as stored/transported; use `TypedStep` after validation for typed config. */
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
  description?: string;
  revision: number;
  steps: Step[];
  edges: Edges;
}

export interface FlowLayout {
  flowId: string;
  positions: Record<StepId, { x: number; y: number }>;
}

/** Step id grammar, shared with the schema (`$defs.step.properties.id.pattern`). */
export const STEP_ID_PATTERN = /^[a-z][a-z0-9_-]*$/;
