// SPDX-License-Identifier: AGPL-3.0-only
// Canvas state: a flow.v1 document held by the shared CommandBus (undo/redo, atomic ops) plus a
// view-only layout. Validation runs on every change using the same validator as the server.
import { CommandBus, type ApplyResult, type Flow, type FlowCommand } from "@nurtuo/flow-schema";
import { validateFlow, type Diagnostic } from "@nurtuo/flow-validate";
import { autoLayout, type Positions } from "./flowAdapter";

export interface FlowState {
  flow: Flow;
  positions: Positions;
  diagnostics: Diagnostic[];
  canUndo: boolean;
  canRedo: boolean;
}

type Listener = () => void;

export class FlowStore {
  private bus: CommandBus;
  private unsub: () => void = () => {};
  private positions: Positions;
  private state: FlowState;
  private listeners = new Set<Listener>();

  constructor(initial: Flow, positions: Positions = {}) {
    this.bus = new CommandBus(initial);
    this.positions = { ...autoLayout(initial), ...positions };
    this.state = this.snapshot();
    this.attach();
  }

  getState = (): FlowState => this.state;

  subscribe = (l: Listener): (() => void) => {
    this.listeners.add(l);
    return () => this.listeners.delete(l);
  };

  dispatch(ops: readonly FlowCommand[]): ApplyResult {
    return this.bus.dispatch(ops);
  }
  undo(): boolean {
    return this.bus.undo();
  }
  redo(): boolean {
    return this.bus.redo();
  }

  /** Layout is view state: it is not part of the flow document or its undo history. */
  setPositions(p: Positions): void {
    this.positions = { ...this.positions, ...p };
    this.refresh();
  }

  /** Replaces the document (clears undo history). Missing positions are auto-laid out. */
  load(flow: Flow, positions?: Positions): void {
    this.unsub();
    this.bus = new CommandBus(flow);
    this.positions = { ...autoLayout(flow), ...(positions ?? {}) };
    this.attach();
    this.refresh();
  }

  private attach(): void {
    this.unsub = this.bus.subscribe(() => this.refresh());
  }

  private snapshot(): FlowState {
    return {
      flow: this.bus.flow,
      positions: this.positions,
      diagnostics: validateFlow(this.bus.flow),
      canUndo: this.bus.canUndo,
      canRedo: this.bus.canRedo,
    };
  }

  private refresh(): void {
    // New steps (add / undo of remove) without a position get one from the auto layout.
    const auto = autoLayout(this.bus.flow);
    const positions = { ...this.positions };
    for (const s of this.bus.flow.steps) positions[s.id] ??= auto[s.id] ?? { x: 0, y: 0 };
    this.positions = positions;
    this.state = this.snapshot();
    this.listeners.forEach((l) => l());
  }
}

const plain = (x: unknown): x is Record<string, unknown> => typeof x === "object" && x !== null && !Array.isArray(x);

/** RFC 7386 patch turning `from` into `to` (removed keys -> null, arrays replaced wholesale). */
export function configPatch(from: Record<string, unknown>, to: Record<string, unknown>): Record<string, unknown> {
  const patch: Record<string, unknown> = {};
  for (const k of Object.keys(from)) if (!(k in to)) patch[k] = null;
  for (const [k, v] of Object.entries(to)) {
    const old = from[k];
    if (JSON.stringify(old) === JSON.stringify(v)) continue;
    patch[k] = plain(old) && plain(v) ? configPatch(old, v) : v;
  }
  return patch;
}
