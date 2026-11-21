// SPDX-License-Identifier: AGPL-3.0-only
// Adapter between the flow.v1 document (source of truth) and React Flow nodes/edges.
// The graph is a pure projection: flow + layout -> graph -> flow is lossless for the logic part.
import { FLOW_SCHEMA_VERSION, type Flow, type FlowLayout, type Step, type StepKind } from "@nurtuo/flow-schema";
import { portsOf } from "@nurtuo/flow-validate";

export type Positions = Record<string, { x: number; y: number }>;

export interface StepNodeData extends Record<string, unknown> {
  kind: StepKind;
  label?: string;
  config: Record<string, unknown>;
  ports: string[];
}

export interface GraphNode {
  id: string;
  type: "step";
  position: { x: number; y: number };
  data: StepNodeData;
}

export interface GraphEdge {
  id: string; // "source:port", the flow.v1 edge key
  source: string;
  sourceHandle: string;
  target: string;
}

export interface Graph {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export type FlowMeta = Pick<Flow, "id" | "name" | "revision"> & { description?: string };

const COL = 240;
const ROW = 120;

/** Layered positions (depth from the start step) for steps that have no stored position. */
export function autoLayout(flow: Flow): Positions {
  const depth = new Map<string, number>();
  const queue = flow.steps.filter((s) => s.kind === "start").map((s) => s.id);
  queue.forEach((id) => depth.set(id, 0));
  while (queue.length > 0) {
    const id = queue.shift() as string;
    for (const [key, to] of Object.entries(flow.edges)) {
      if (key.slice(0, key.indexOf(":")) === id && !depth.has(to)) {
        depth.set(to, (depth.get(id) ?? 0) + 1);
        queue.push(to);
      }
    }
  }
  const rows = new Map<number, number>();
  const out: Positions = {};
  for (const s of flow.steps) {
    const d = depth.get(s.id) ?? Math.max(-1, ...depth.values()) + 1;
    const r = rows.get(d) ?? 0;
    rows.set(d, r + 1);
    out[s.id] = { x: r * COL, y: d * ROW };
  }
  return out;
}

export function flowToGraph(flow: Flow, positions: Positions = {}): Graph {
  const auto = autoLayout(flow);
  const nodes: GraphNode[] = flow.steps.map((s) => ({
    id: s.id,
    type: "step",
    position: positions[s.id] ?? auto[s.id] ?? { x: 0, y: 0 },
    data: {
      kind: s.kind,
      ...(s.label !== undefined ? { label: s.label } : {}),
      config: s.config,
      ports: portsOf(s),
    },
  }));
  const edges: GraphEdge[] = Object.entries(flow.edges).map(([key, target]) => {
    const i = key.indexOf(":");
    return { id: key, source: key.slice(0, i), sourceHandle: key.slice(i + 1), target };
  });
  return { nodes, edges };
}

export function graphToFlow(meta: FlowMeta, graph: Graph): Flow {
  const steps: Step[] = graph.nodes.map((n) => ({
    id: n.id,
    kind: n.data.kind,
    ...(n.data.label !== undefined ? { label: n.data.label } : {}),
    config: n.data.config,
  }));
  const edges: Record<string, string> = {};
  for (const e of graph.edges) edges[`${e.source}:${e.sourceHandle}`] = e.target;
  return {
    schema: FLOW_SCHEMA_VERSION,
    id: meta.id,
    name: meta.name,
    ...(meta.description !== undefined ? { description: meta.description } : {}),
    revision: meta.revision,
    steps,
    edges: edges as Flow["edges"],
  };
}

export function graphToPositions(graph: Graph): Positions {
  return Object.fromEntries(graph.nodes.map((n) => [n.id, { x: n.position.x, y: n.position.y }]));
}

export function flowMeta(flow: Flow): FlowMeta {
  return {
    id: flow.id,
    name: flow.name,
    revision: flow.revision,
    ...(flow.description !== undefined ? { description: flow.description } : {}),
  };
}

// ---- JSON load / save -----------------------------------------------------------------------

export interface Loaded {
  flow?: Flow;
  layout?: FlowLayout;
}

/** Parses a flow.v1 document or a layout file. Semantic problems are left to the validator. */
export function parseLoaded(text: string): Loaded {
  const v: unknown = JSON.parse(text);
  if (typeof v !== "object" || v === null || Array.isArray(v)) throw new Error("Expected a JSON object");
  const o = v as Record<string, unknown>;
  if (Array.isArray(o["steps"]) && typeof o["edges"] === "object" && o["edges"] !== null) return { flow: v as Flow };
  if (typeof o["positions"] === "object" && o["positions"] !== null) return { layout: v as FlowLayout };
  throw new Error("Not a flow.v1 document (needs steps and edges) or a layout (needs positions)");
}

export const serializeFlow = (flow: Flow): string => JSON.stringify(flow, null, 2) + "\n";
export const serializeLayout = (flowId: string, positions: Positions): string =>
  JSON.stringify({ flowId, positions } satisfies FlowLayout, null, 2) + "\n";
