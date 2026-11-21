// SPDX-License-Identifier: Apache-2.0
// Shared flow validator: runs identically in the canvas, control plane, CLI and MCP server, and
// is kept in lockstep with the Go validator (sdk/go/flow) by the conformance corpus in
// sdk/ts/flow-schema/testdata/conformance.
//
// Two stages. Stage 1 checks the document against flow.v1.schema.json (codes `schema.*`). If it
// finds errors, stage 2 is skipped (semantic rules assume a well-formed flow). Stage 2 applies
// the rules documented in docs/flow-validation.md. Output is de-duplicated and sorted by
// (path, code, stepId) so both implementations agree on order.
import { flowSchema, type Step } from "@nurtuo/flow-schema";
import { checkSchema, escapePointer } from "./jsonschema.js";

export interface Diagnostic {
  severity: "error" | "warning";
  /** Stable machine-readable code, e.g. `edge.target_unknown`, `schema.required`. */
  code: string;
  message: string;
  /** JSON Pointer into the flow document ("" is the root). May point at a not-yet-existing edge. */
  path: string;
  stepId?: string;
}
/** @deprecated use Diagnostic */
export type Issue = Diagnostic;

export function hasErrors(diags: readonly Diagnostic[]): boolean {
  return diags.some((d) => d.severity === "error");
}

/** Output ports of a step, in canonical order. Terminal kinds have none. */
export function portsOf(step: Step): string[] {
  const cfg = step.config as Record<string, unknown>;
  switch (step.kind) {
    case "decision": return ["true", "false"];
    case "tryCatch": return ["try", "catch"];
    case "stop":
    case "returnDocuments": return [];
    case "branch": {
      const n = typeof cfg["paths"] === "number" ? cfg["paths"] : 0;
      return Array.from({ length: n }, (_, i) => String(i + 1));
    }
    case "route": {
      const routes = Array.isArray(cfg["routes"]) ? cfg["routes"] : [];
      const ports: string[] = [];
      for (const r of routes) {
        const id = (r as { id?: unknown } | null)?.id;
        if (typeof id === "string" && id !== "default" && !ports.includes(id)) ports.push(id);
      }
      return [...ports, "default"];
    }
    default: return ["default"];
  }
}

interface Loose {
  flowId: string;
  steps: Step[];
  edges: Record<string, string>;
}

export function validateFlow(input: unknown): Diagnostic[] {
  const out: Diagnostic[] = [];
  const raw = checkSchema(flowSchema, input);
  if (raw.length > 0) {
    const steps = (input as { steps?: unknown } | null)?.steps;
    for (const r of raw) {
      const d: Diagnostic = { severity: "error", code: r.code, message: r.message, path: r.path };
      const m = /^\/steps\/(\d+)(\/|$)/.exec(r.path);
      if (m && Array.isArray(steps)) {
        const id = (steps[Number(m[1])] as { id?: unknown } | null)?.id;
        if (typeof id === "string") d.stepId = id;
      }
      out.push(d);
    }
  } else {
    const f = input as { id: string; steps: Step[]; edges: Record<string, string> };
    semantic({ flowId: f.id, steps: f.steps, edges: f.edges }, out);
  }
  return finish(out);
}

function finish(diags: Diagnostic[]): Diagnostic[] {
  const seen = new Set<string>();
  const uniq = diags.filter((d) => {
    const k = JSON.stringify([d.severity, d.code, d.path, d.stepId ?? ""]);
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
  const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);
  return uniq.sort((a, b) => cmp(a.path, b.path) || cmp(a.code, b.code) || cmp(a.stepId ?? "", b.stepId ?? ""));
}

function semantic(f: Loose, out: Diagnostic[]): void {
  const err = (code: string, path: string, message: string, stepId?: string) =>
    out.push({ severity: "error", code, path, message, ...(stepId !== undefined ? { stepId } : {}) });
  const warn = (code: string, path: string, message: string, stepId?: string) =>
    out.push({ severity: "warning", code, path, message, ...(stepId !== undefined ? { stepId } : {}) });

  const first = new Map<string, Step>(); // id -> first step with that id
  f.steps.forEach((s, i) => {
    if (first.has(s.id)) err("step.duplicate_id", `/steps/${i}`, `Duplicate step id ${s.id}`, s.id);
    else first.set(s.id, s);
  });

  const starts = f.steps.flatMap((s, i) => (s.kind === "start" ? [i] : []));
  if (starts.length === 0) err("flow.start_count", "/steps", "Flow has no start step");
  for (const i of starts.slice(1)) err("flow.start_count", `/steps/${i}`, "Flow has more than one start step", f.steps[i]?.id);

  const adj = new Map<string, string[]>();
  for (const [key, target] of Object.entries(f.edges)) {
    const c = key.indexOf(":");
    const src = key.slice(0, c);
    const port = key.slice(c + 1);
    const path = `/edges/${escapePointer(key)}`;
    const srcStep = first.get(src);
    const tgtStep = first.get(target);
    if (!srcStep) err("edge.source_unknown", path, `Edge ${key} has unknown source step ${src}`);
    else if (!portsOf(srcStep).includes(port)) {
      err("edge.port_invalid", path, `Step ${src} (${srcStep.kind}) has no port ${port}`, src);
    }
    if (!tgtStep) err("edge.target_unknown", path, `Edge ${key} points to unknown step ${target}`, srcStep ? src : undefined);
    else if (tgtStep.kind === "start") err("start.has_incoming", path, `Start step ${target} cannot have incoming edges`, target);
    if (srcStep && tgtStep) {
      const l = adj.get(src) ?? [];
      l.push(target);
      adj.set(src, l);
    }
  }

  f.steps.forEach((s, i) => {
    const at = `/steps/${i}`;
    for (const p of portsOf(s)) {
      if (f.edges[`${s.id}:${p}`] === undefined) {
        err("step.port_unconnected", `/edges/${escapePointer(`${s.id}:${p}`)}`, `Port ${p} of step ${s.id} is not connected`, s.id);
      }
    }
    const cfg = s.config as Record<string, any>; // eslint-disable-line @typescript-eslint/no-explicit-any
    if (s.kind === "route") {
      const seen = new Set<string>();
      (cfg["routes"] as { id: string }[]).forEach((r, j) => {
        if (r.id === "default") err("route.reserved_id", `${at}/config/routes/${j}/id`, "Route id 'default' is reserved", s.id);
        else if (seen.has(r.id)) err("route.duplicate_id", `${at}/config/routes/${j}/id`, `Duplicate route id ${r.id}`, s.id);
        seen.add(r.id);
      });
    } else if (s.kind === "setProperties") {
      const seen = new Set<string>();
      (cfg["properties"] as { name: string; scope?: string }[]).forEach((p, j) => {
        const k = JSON.stringify([p.scope ?? "document", p.name]);
        if (seen.has(k)) err("setProperties.duplicate_name", `${at}/config/properties/${j}/name`, `Property ${p.name} is set twice in the same scope`, s.id);
        seen.add(k);
      });
    } else if (s.kind === "processCall" && cfg["flow"] === f.flowId) {
      err("processCall.self_reference", `${at}/config/flow`, "A flow cannot call itself", s.id);
    }
  });

  // Cycles: the M1 engine interprets a DAG. A step is on a cycle iff it can reach itself.
  const reach = (from: string): Set<string> => {
    const seen = new Set<string>();
    const stack = [...(adj.get(from) ?? [])];
    while (stack.length > 0) {
      const n = stack.pop() as string;
      if (seen.has(n)) continue;
      seen.add(n);
      stack.push(...(adj.get(n) ?? []));
    }
    return seen;
  };
  f.steps.forEach((s, i) => {
    if (reach(s.id).has(s.id)) err("flow.cycle", `/steps/${i}`, `Step ${s.id} is part of a cycle`, s.id);
  });

  if (starts.length > 0) {
    const reached = new Set<string>();
    for (const i of starts) {
      const id = f.steps[i]?.id as string;
      reached.add(id);
      for (const n of reach(id)) reached.add(n);
    }
    f.steps.forEach((s, i) => {
      if (!reached.has(s.id)) warn("step.unreachable", `/steps/${i}`, `Step ${s.id} is not reachable from the start step`, s.id);
    });
  }
}
