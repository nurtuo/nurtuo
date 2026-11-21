// SPDX-License-Identifier: Apache-2.0
// CommandBus operations: the canvas, the MCP `apply_ops` tool and the AI agent all mutate flows
// through the same commands. `applyOps` is pure and atomic (all ops apply or none do); it checks
// referential integrity only. Whether the result is a *valid* flow is the validator's job.
import { STEP_ID_PATTERN, type Flow, type Port, type Step, type StepId } from "./types.js";

export type FlowCommand =
  /** Appends a step. With `after`, connects `after.step:after.port` to it; if that port was
   *  already connected the new step is spliced in (its `default` port takes the old target). */
  | { op: "addStep"; step: Step; after?: { step: StepId; port: Port } }
  /** Removes a step and every edge from or to it. */
  | { op: "removeStep"; id: StepId }
  /** RFC 7386 merge patch on `config`: objects merge, `null` deletes a key, arrays replace. */
  | { op: "updateConfig"; id: StepId; patch: Record<string, unknown> }
  | { op: "connect"; from: StepId; port: Port; to: StepId }
  | { op: "disconnect"; from: StepId; port: Port }
  /** `null` clears the label. */
  | { op: "setLabel"; id: StepId; label: string | null }
  /** Renames a step and rewrites every edge that references it. */
  | { op: "renameStep"; id: StepId; newId: StepId };

export type CommandErrorCode =
  | "step_exists" | "step_not_found" | "invalid_id" | "port_occupied" | "edge_not_found" | "unknown_op";

export type ApplyResult =
  | { ok: true; flow: Flow }
  | { ok: false; index: number; code: CommandErrorCode; message: string };

class OpError extends Error {
  constructor(readonly code: CommandErrorCode, message: string) {
    super(message);
  }
}

const key = (id: StepId, port: Port) => `${id}:${port}` as const;
const DEFAULT_ONLY_KINDS = new Set(["connector", "queue", "map", "script", "message", "setProperties", "processCall", "notify"]);

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function mergePatch(target: Record<string, unknown>, patch: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = { ...target };
  for (const [k, v] of Object.entries(patch)) {
    if (v === null) delete out[k];
    else if (isPlainObject(v) && isPlainObject(out[k])) out[k] = mergePatch(out[k] as Record<string, unknown>, v);
    else out[k] = structuredClone(v);
  }
  return out;
}

function stepIndex(flow: Flow, id: StepId): number {
  const i = flow.steps.findIndex((s) => s.id === id);
  if (i < 0) throw new OpError("step_not_found", `No step with id ${id}`);
  return i;
}

function apply(flow: Flow, c: FlowCommand): void {
  switch (c.op) {
    case "addStep": {
      if (!STEP_ID_PATTERN.test(c.step.id)) throw new OpError("invalid_id", `Invalid step id ${c.step.id}`);
      if (flow.steps.some((s) => s.id === c.step.id)) throw new OpError("step_exists", `Step ${c.step.id} already exists`);
      if (c.after) stepIndex(flow, c.after.step);
      const k = c.after ? key(c.after.step, c.after.port) : undefined;
      const old = k === undefined ? undefined : flow.edges[k];
      if (old !== undefined && !DEFAULT_ONLY_KINDS.has(c.step.kind)) {
        throw new OpError("port_occupied", `${k} is already connected and ${c.step.kind} cannot be spliced in`);
      }
      flow.steps.push(structuredClone(c.step));
      if (k !== undefined) {
        flow.edges[k] = c.step.id;
        if (old !== undefined) flow.edges[key(c.step.id, "default")] = old;
      }
      return;
    }
    case "removeStep": {
      const i = stepIndex(flow, c.id);
      flow.steps.splice(i, 1);
      for (const [k, target] of Object.entries(flow.edges)) {
        if (target === c.id || k.slice(0, k.indexOf(":")) === c.id) delete flow.edges[k as keyof typeof flow.edges];
      }
      return;
    }
    case "updateConfig": {
      const s = flow.steps[stepIndex(flow, c.id)] as Step;
      s.config = mergePatch(s.config, c.patch);
      return;
    }
    case "connect": {
      stepIndex(flow, c.from);
      stepIndex(flow, c.to);
      const k = key(c.from, c.port);
      if (flow.edges[k] !== undefined) throw new OpError("port_occupied", `${k} is already connected to ${flow.edges[k]}`);
      flow.edges[k] = c.to;
      return;
    }
    case "disconnect": {
      const k = key(c.from, c.port);
      if (flow.edges[k] === undefined) throw new OpError("edge_not_found", `No edge ${k}`);
      delete flow.edges[k];
      return;
    }
    case "setLabel": {
      const s = flow.steps[stepIndex(flow, c.id)] as Step;
      if (c.label === null) delete s.label;
      else s.label = c.label;
      return;
    }
    case "renameStep": {
      const s = flow.steps[stepIndex(flow, c.id)] as Step;
      if (!STEP_ID_PATTERN.test(c.newId)) throw new OpError("invalid_id", `Invalid step id ${c.newId}`);
      if (c.newId !== c.id && flow.steps.some((o) => o.id === c.newId)) throw new OpError("step_exists", `Step ${c.newId} already exists`);
      s.id = c.newId;
      const next: Record<string, string> = {};
      for (const [k, target] of Object.entries(flow.edges)) {
        const i = k.indexOf(":");
        const src = k.slice(0, i);
        next[`${src === c.id ? c.newId : src}${k.slice(i)}`] = target === c.id ? c.newId : target;
      }
      flow.edges = next;
      return;
    }
    default:
      throw new OpError("unknown_op", `Unknown op ${(c as { op?: string }).op}`);
  }
}

/** Applies `ops` in order to a copy of `flow`. The input is never mutated; on the first failing
 *  op nothing is applied and the failing index is reported. */
export function applyOps(flow: Flow, ops: readonly FlowCommand[]): ApplyResult {
  const draft = structuredClone(flow);
  for (let index = 0; index < ops.length; index++) {
    try {
      apply(draft, ops[index] as FlowCommand);
    } catch (e) {
      if (e instanceof OpError) return { ok: false, index, code: e.code, message: e.message };
      throw e;
    }
  }
  return { ok: true, flow: draft };
}

/** Holds the current flow, applies command batches atomically, and keeps an undo/redo history. */
export class CommandBus {
  #flow: Flow;
  #undo: Flow[] = [];
  #redo: Flow[] = [];
  #listeners = new Set<(flow: Flow) => void>();

  constructor(initial: Flow) {
    this.#flow = initial;
  }

  get flow(): Flow { return this.#flow; }
  get canUndo(): boolean { return this.#undo.length > 0; }
  get canRedo(): boolean { return this.#redo.length > 0; }

  dispatch(ops: readonly FlowCommand[]): ApplyResult {
    const r = applyOps(this.#flow, ops);
    if (r.ok) {
      this.#undo.push(this.#flow);
      this.#redo = [];
      this.#flow = r.flow;
      this.#emit();
    }
    return r;
  }

  undo(): boolean {
    const prev = this.#undo.pop();
    if (!prev) return false;
    this.#redo.push(this.#flow);
    this.#flow = prev;
    this.#emit();
    return true;
  }

  redo(): boolean {
    const next = this.#redo.pop();
    if (!next) return false;
    this.#undo.push(this.#flow);
    this.#flow = next;
    this.#emit();
    return true;
  }

  /** Returns an unsubscribe function. */
  subscribe(fn: (flow: Flow) => void): () => void {
    this.#listeners.add(fn);
    return () => { this.#listeners.delete(fn); };
  }

  #emit(): void {
    for (const fn of this.#listeners) fn(this.#flow);
  }
}
