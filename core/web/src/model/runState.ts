// SPDX-License-Identifier: AGPL-3.0-only
// Result of a canvas test run. Held in memory only and cleared as soon as the flow changes.
import type { Flow } from "@nurtuo/flow-schema";
import type { RunDocument, TestRunResponse } from "../api/controlPlane";

export type StepStatus = "ok" | "error" | "stopped" | "not_run";

export type RunState =
  | { phase: "idle" }
  | { phase: "running"; flowHash: string }
  | { phase: "done"; flowHash: string; result: TestRunResponse }
  | { phase: "error"; flowHash: string; message: string };

export const IDLE: RunState = { phase: "idle" };

export const hashFlow = (flow: Flow): string => JSON.stringify(flow);

/** Any edit of the flow invalidates colours and results. A run in flight stays (its result is dropped on arrival). */
export function reconcile(state: RunState, flow: Flow): RunState {
  if (state.phase === "idle") return state;
  return state.flowHash === hashFlow(flow) ? state : IDLE;
}

/** Per-step status for the nodes; empty when there is no result for the current flow. */
export function stepStatuses(state: RunState, flow: Flow): Map<string, StepStatus> {
  const m = new Map<string, StepStatus>();
  if (state.phase !== "done" || state.flowHash !== hashFlow(flow)) return m;
  const byId = new Map(state.result.steps.map((s) => [s.id, s.status]));
  for (const s of flow.steps) m.set(s.id, byId.get(s.id) ?? "not_run");
  return m;
}

export const STATUS_LABEL: Record<StepStatus, string> = { ok: "ok", error: "error", stopped: "stopped", not_run: "not run" };

export const PREVIEW_LIMIT = 64 * 1024;

export function decodeBase64(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export type Preview = { kind: "text"; text: string; truncated: boolean; size: number } | { kind: "binary"; size: number };

/** UTF-8 text preview up to 64 KiB, otherwise "binary". */
export function previewOf(doc: RunDocument): Preview {
  const bytes = decodeBase64(doc.payload);
  try {
    const truncated = bytes.length > PREVIEW_LIMIT;
    const slice = bytes.subarray(0, PREVIEW_LIMIT);
    // A cut in the middle of a multi-byte sequence must not make a text file look binary.
    const text = new TextDecoder("utf-8", { fatal: true }).decode(slice, { stream: truncated });
    if (text.includes("\u0000")) return { kind: "binary", size: bytes.length };
    return { kind: "text", text, truncated, size: bytes.length };
  } catch {
    return { kind: "binary", size: bytes.length };
  }
}

/** Hint line for a failed run (spec "Failure modes"). */
export function failureHint(result: TestRunResponse): string | null {
  const m = result.error?.message ?? "";
  if (result.error?.code === "output_too_large") return "Output larger than 1 MiB; use `nurtuo flow run`.";
  if (m.includes("host key mismatch")) return "The server's key changed; verify before trusting the new key.";
  return null;
}
