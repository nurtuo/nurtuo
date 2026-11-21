// SPDX-License-Identifier: AGPL-3.0-only
// Semantic, readable view of a flow for agents: steps with ports and where each port leads.
import type { Flow } from "@nurtuo/flow-schema";
import { portsOf } from "@nurtuo/flow-validate";

export interface StepView {
  id: string;
  kind: string;
  label?: string;
  config: Record<string, unknown>;
  ports: { port: string; to: string | null }[];
}

export interface FlowView {
  id: string;
  name: string;
  revision: number;
  steps: StepView[];
  /** One line per edge, `from:port -> to`. */
  edges: string[];
  summary: string;
}

export function flowView(flow: Flow): FlowView {
  const steps = flow.steps.map((s): StepView => ({
    id: s.id,
    kind: s.kind,
    ...(s.label !== undefined ? { label: s.label } : {}),
    config: s.config,
    ports: portsOf(s).map((port) => ({ port, to: flow.edges[`${s.id}:${port}`] ?? null })),
  }));
  const edges = Object.entries(flow.edges).map(([k, to]) => `${k} -> ${to}`);
  const summary = steps
    .map((s) => `${s.id} (${s.kind}${s.label ? `: ${s.label}` : ""})` +
      (s.ports.length ? ` => ${s.ports.map((p) => `${p.port}:${p.to ?? "?"}`).join(", ")}` : " [end]"))
    .join("\n");
  return { id: flow.id, name: flow.name, revision: flow.revision, steps, edges, summary };
}
