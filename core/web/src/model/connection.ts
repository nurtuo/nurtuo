// SPDX-License-Identifier: AGPL-3.0-only
import type { Flow } from "@nurtuo/flow-schema";
import { portsOf } from "@nurtuo/flow-validate";

export interface ConnectionCheck {
  ok: boolean;
  reason?: string;
}

/** Whether `source:port -> target` may be added. Mirrors the validator rules (ports, start, cycles). */
export function checkConnection(flow: Flow, c: { source: string; port: string; target: string }): ConnectionCheck {
  const src = flow.steps.find((s) => s.id === c.source);
  const tgt = flow.steps.find((s) => s.id === c.target);
  if (!src || !tgt) return { ok: false, reason: "Unknown step" };
  if (!portsOf(src).includes(c.port)) return { ok: false, reason: `${src.kind} has no port ${c.port}` };
  if (flow.edges[`${c.source}:${c.port}`] !== undefined) return { ok: false, reason: "Port already connected" };
  if (tgt.kind === "start") return { ok: false, reason: "Start cannot have incoming edges" };
  // A cycle appears iff the target can already reach the source.
  const seen = new Set<string>();
  const stack = [c.target];
  while (stack.length > 0) {
    const id = stack.pop() as string;
    if (id === c.source) return { ok: false, reason: "Would create a cycle" };
    if (seen.has(id)) continue;
    seen.add(id);
    for (const [k, to] of Object.entries(flow.edges)) if (k.slice(0, k.indexOf(":")) === id) stack.push(to);
  }
  return { ok: true };
}
