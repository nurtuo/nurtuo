// SPDX-License-Identifier: AGPL-3.0-only
// Palette and per-kind config schemas, derived from the flow.v1 JSON Schema.
import { STEP_KINDS, STEP_ID_PATTERN, flowSchema, type Flow, type Step, type StepKind } from "@nurtuo/flow-schema";

export interface KindDef {
  kind: StepKind;
  label: string;
  description: string;
}

export type Schema = Record<string, any>; // eslint-disable-line @typescript-eslint/no-explicit-any

const defs = (flowSchema["$defs"] ?? {}) as Record<string, Schema>;

export const configSchemaOf = (kind: StepKind): Schema => defs[`${kind}Config`] ?? { type: "object" };

const humanize = (k: string) => k.replace(/([A-Z])/g, " $1").replace(/^./, (c) => c.toUpperCase());

export const STEP_CATALOG: readonly KindDef[] = STEP_KINDS.map((kind) => ({
  kind,
  label: humanize(kind),
  description: String(configSchemaOf(kind)["description"] ?? ""),
}));

export const DND_MIME = "application/x-nurtuo-step";
export const GRID = 16;

/** Smallest config that satisfies the required shape (values may still fail validation). */
export function defaultConfig(kind: StepKind): Record<string, unknown> {
  switch (kind) {
    case "start": return { type: "manual" };
    case "queue": return { queue: "", operation: "publish" };
    case "connector": return { connector: "", operation: "" };
    case "map": return { jsonata: "" };
    case "message": return { template: "" };
    case "setProperties": return { properties: [] };
    case "decision": return { cel: "" };
    case "branch": return { paths: 2 };
    case "route": return { routes: [] };
    case "processCall": return { flow: "" };
    case "notify": return { channel: "" };
    default: return {};
  }
}

/** Unique, schema-valid step id for a new step of `kind`. */
export function newStepId(flow: Flow, kind: StepKind): string {
  const base = kind.toLowerCase();
  const taken = new Set(flow.steps.map((s) => s.id));
  for (let i = 1; ; i++) {
    const id = `${base}-${i}`;
    if (!taken.has(id) && STEP_ID_PATTERN.test(id)) return id;
  }
}

export function newStep(flow: Flow, kind: StepKind): Step {
  return { id: newStepId(flow, kind), kind, config: defaultConfig(kind) };
}

export const emptyFlow = (): Flow => ({
  schema: "nurtuo.flow/v1",
  id: "new-flow",
  name: "New flow",
  revision: 1,
  steps: [],
  edges: {},
});
