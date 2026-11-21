// SPDX-License-Identifier: Apache-2.0
import type { Flow } from "@nurtuo/flow-schema";

export interface Issue {
  severity: "error" | "warning";
  stepId?: string;
  code: string;
  message: string;
}

/** Validates a flow. The same function runs in the canvas, control plane, CLI and MCP server. */
export function validateFlow(flow: Flow): Issue[] {
  const issues: Issue[] = [];
  const ids = new Set<string>();
  for (const s of flow.steps) {
    if (ids.has(s.id)) issues.push({ severity: "error", stepId: s.id, code: "duplicate_id", message: `Duplicate step id ${s.id}` });
    ids.add(s.id);
  }
  const starts = flow.steps.filter((s) => s.kind === "start").length;
  if (starts !== 1) issues.push({ severity: "error", code: "start_count", message: `Expected exactly one start step, found ${starts}` });
  for (const [key, target] of Object.entries(flow.edges)) {
    const source = key.split(":")[0] ?? "";
    if (!ids.has(source)) issues.push({ severity: "error", code: "edge_source", message: `Edge ${key} has unknown source` });
    if (!ids.has(target)) issues.push({ severity: "error", code: "edge_target", message: `Edge ${key} points to unknown step ${target}` });
  }
  return issues;
}
