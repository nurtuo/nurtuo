// SPDX-License-Identifier: AGPL-3.0-only
// Pure helpers behind `suggest_next_steps`, `explain_diagnostic` and `diff_flows`.
import type { Flow, Step, StepKind } from "@nurtuo/flow-schema";
import { hasErrors, portsOf, validateFlow, type Diagnostic } from "@nurtuo/flow-validate";
import { explainCode, kindExample, kindRole } from "./catalog.js";
import { isSecretKey, REDACTED } from "./guardrails.js";

type Json = Record<string, unknown>;

// ---------------------------------------------------------------- suggest_next_steps

export interface Candidate {
  kind: StepKind;
  why: string;
  /** Ready-to-send op. Config values are placeholders; adapt them to the user's goal. */
  op: Json;
}
export interface Suggestion {
  at: { step: string; port: string };
  after: string;
  candidates: Candidate[];
}

const idBase: Record<StepKind, string> = {
  start: "start", connector: "call", queue: "publish", map: "transform", script: "script", message: "message", setProperties: "set_props",
  decision: "check", branch: "branch", route: "route", tryCatch: "guard", processCall: "subflow", notify: "notify",
  stop: "stop", returnDocuments: "return",
};

export function uniqueId(flow: Flow, base: string, taken: Set<string> = new Set()): string {
  const ids = new Set([...flow.steps.map((s) => s.id), ...taken]);
  if (!ids.has(base)) return base;
  for (let i = 2; ; i++) if (!ids.has(`${base}_${i}`)) return `${base}_${i}`;
}

function candidate(flow: Flow, kind: StepKind, at: { step: string; port: string }, why: string, taken: Set<string>, config?: Json): Candidate {
  const id = uniqueId(flow, idBase[kind], taken);
  taken.add(id);
  return { kind, why, op: { op: "addStep", after: at, step: { id, kind, config: config ?? kindExample(kind) } } };
}

export function openPorts(flow: Flow): { step: Step; port: string }[] {
  return flow.steps.flatMap((step) => portsOf(step).filter((port) => flow.edges[`${step.id}:${port}`] === undefined).map((port) => ({ step, port })));
}

export function suggestNextSteps(flow: Flow) {
  const diagnostics = validateFlow(flow);
  const open = openPorts(flow);
  const taken = new Set<string>();
  const hasTerminal = flow.steps.some((s) => kindRole(s.kind) === "terminal");
  const bigEnough = flow.steps.length >= 4;
  const suggestions: Suggestion[] = [];

  if (!flow.steps.some((s) => s.kind === "start")) {
    const id = uniqueId(flow, "start");
    return {
      valid: false, openPorts: [], diagnostics,
      suggestions: [] as Suggestion[],
      firstStep: { why: "A flow needs exactly one start step.", op: { op: "addStep", step: { id, kind: "start", config: kindExample("start") } } },
      fixes: fixesFor(diagnostics),
      done: false,
    };
  }

  for (const { step, port } of open) {
    const at = { step: step.id, port };
    const c: Candidate[] = [];
    const finish = () => c.push(candidate(flow, "returnDocuments", at, "End the flow and return the current document.", taken));
    switch (step.kind) {
      case "start":
        c.push(candidate(flow, "connector", at, "Fetch or process data with a connector (see list_connectors).", taken));
        c.push(candidate(flow, "map", at, "Reshape the incoming document.", taken));
        break;
      case "decision":
        if (port === "true") {
          c.push(candidate(flow, "map", at, "Continue the happy path by reshaping the document.", taken));
          finish();
        } else {
          c.push(candidate(flow, "stop", at, "End the run when the condition is not met.", taken, { status: 404, message: "not found" }));
          c.push(candidate(flow, "notify", at, "Alert somebody that the condition failed.", taken));
        }
        break;
      case "tryCatch":
        if (port === "try") c.push(candidate(flow, "connector", at, "Put the fallible operation inside the try path.", taken));
        else {
          c.push(candidate(flow, "notify", at, "Tell somebody the try path failed.", taken));
          c.push(candidate(flow, "stop", at, "End the run after the failure.", taken, { status: 500, message: "failed" }));
        }
        break;
      case "connector":
        if (bigEnough) finish();
        c.push(candidate(flow, "map", at, "Transform the connector's output.", taken));
        c.push(candidate(flow, "decision", at, "Branch on the result (e.g. record found?).", taken));
        if (!bigEnough) finish();
        break;
      case "map":
      case "message":
      case "setProperties":
        if (bigEnough) finish();
        c.push(candidate(flow, "connector", at, "Deliver the result to a target system (e.g. SFTP CREATE, HTTP POST).", taken));
        if (!bigEnough) finish();
        break;
      default: // branch / route ports, queue, notify, processCall
        finish();
        c.push(candidate(flow, "map", at, "Transform before continuing.", taken));
        c.push(candidate(flow, "stop", at, "End this path without returning documents.", taken, {}));
    }
    suggestions.push({ at, after: step.id, candidates: c });
  }

  const unreachable = diagnostics.filter((d) => d.code === "step.unreachable");
  const connectHints = unreachable.flatMap((d) => open.slice(0, 1).map((o) => ({
    why: `Step ${d.stepId} is not reachable; connect it from an open port.`,
    op: { op: "connect", from: o.step.id, port: o.port, to: d.stepId },
  })));

  return {
    valid: !hasErrors(diagnostics),
    /** True when nothing is left to connect and the flow validates. */
    done: !hasErrors(diagnostics) && open.length === 0,
    openPorts: open.map((o) => ({ step: o.step.id, kind: o.step.kind, port: o.port })),
    suggestions,
    ...(connectHints.length ? { connectHints } : {}),
    ...(open.length === 0 && !hasTerminal && flow.steps.length > 0 ? { note: "The flow has no terminal step; add returnDocuments or stop." } : {}),
    diagnostics,
    fixes: fixesFor(diagnostics),
  };
}

function fixesFor(diags: Diagnostic[]) {
  const seen = new Set<string>();
  return diags.filter((d) => d.severity === "error" && !seen.has(d.code) && seen.add(d.code)).map((d) => {
    const h = explainCode(d.code);
    return { code: d.code, fix: h?.fix ?? "See explain_diagnostic." };
  });
}

// ---------------------------------------------------------------- explain_diagnostic

export function explainDiagnostic(d: { code: string; path?: string; stepId?: string; message?: string }, flow?: Flow) {
  const help = explainCode(d.code);
  const out: Json = {
    code: d.code,
    known: help !== undefined,
    title: help?.title ?? "Unknown diagnostic code",
    explanation: help?.explanation ?? "This code is not in the built-in catalog. Re-run validate and read the message and path.",
    howToFix: help?.fix ?? "Use get_flow to inspect the flow and validate to see current diagnostics.",
  };
  if (!flow) return out;
  const step = d.stepId ? flow.steps.find((s) => s.id === d.stepId) : undefined;
  const ops: Json[] = [];
  if (step) {
    out["step"] = { id: step.id, kind: step.kind, ports: portsOf(step), config: step.config };
    if (d.code === "step.port_unconnected") {
      const m = /\/edges\/(.+)$/.exec(d.path ?? "");
      const port = m ? decodeURIComponent(m[1] as string).replace(/~1/g, "/").replace(/~0/g, "~").split(":").slice(1).join(":") : portsOf(step).find((p) => flow.edges[`${step.id}:${p}`] === undefined);
      if (port) ops.push({ op: "addStep", after: { step: step.id, port }, step: { id: uniqueId(flow, "return"), kind: "returnDocuments", config: {} } });
    } else if (d.code === "flow.cycle" || d.code === "edge.port_invalid") {
      ops.push({ op: "disconnect", from: step.id, port: portsOf(step)[0] ?? "default" });
    } else if (d.code === "step.unreachable") {
      const o = openPorts(flow)[0];
      if (o) ops.push({ op: "connect", from: o.step.id, port: o.port, to: step.id });
      ops.push({ op: "removeStep", id: step.id });
    }
  }
  if (ops.length) out["exampleOps"] = ops;
  return out;
}

// ---------------------------------------------------------------- diff_flows

export interface ValueChange { path: string; before?: unknown; after?: unknown }

/** A change record whose values are masked when the leaf key is a secret (the `path` hides the key from redact()). */
function change(path: string, before?: unknown, after?: unknown): ValueChange {
  const secret = isSecretKey(path.slice(path.lastIndexOf("/") + 1));
  const m = (v: unknown) => (secret && v !== undefined && v !== "" ? REDACTED : v);
  return { path, ...(before !== undefined ? { before: m(before) } : {}), ...(after !== undefined ? { after: m(after) } : {}) };
}

function diffValue(a: unknown, b: unknown, path: string, out: ValueChange[]): void {
  if (JSON.stringify(a) === JSON.stringify(b)) return;
  const isObj = (v: unknown): v is Json => !!v && typeof v === "object" && !Array.isArray(v);
  if (isObj(a) && isObj(b)) {
    for (const k of [...new Set([...Object.keys(a), ...Object.keys(b)])].sort()) {
      if (!(k in a)) out.push(change(`${path}/${k}`, undefined, b[k]));
      else if (!(k in b)) out.push(change(`${path}/${k}`, a[k]));
      else diffValue(a[k], b[k], `${path}/${k}`, out);
    }
  } else out.push(change(path, a, b));
}

export function diffFlows(a: Flow, b: Flow) {
  const aSteps = new Map(a.steps.map((s) => [s.id, s]));
  const bSteps = new Map(b.steps.map((s) => [s.id, s]));
  const stepsAdded = b.steps.filter((s) => !aSteps.has(s.id)).map((s) => ({ id: s.id, kind: s.kind }));
  const stepsRemoved = a.steps.filter((s) => !bSteps.has(s.id)).map((s) => ({ id: s.id, kind: s.kind }));
  const stepsChanged: { id: string; changes: ValueChange[] }[] = [];
  for (const s of b.steps) {
    const before = aSteps.get(s.id);
    if (!before) continue;
    const changes: ValueChange[] = [];
    diffValue({ kind: before.kind, label: before.label, config: before.config }, { kind: s.kind, label: s.label, config: s.config }, "", changes);
    if (changes.length) stepsChanged.push({ id: s.id, changes });
  }
  const edgesAdded = Object.entries(b.edges).filter(([k]) => !(k in a.edges)).map(([k, to]) => `${k} -> ${to}`);
  const edgesRemoved = Object.entries(a.edges).filter(([k]) => !(k in b.edges)).map(([k, to]) => `${k} -> ${to}`);
  const edgesRetargeted = Object.entries(b.edges).filter(([k, to]) => k in a.edges && a.edges[k as keyof typeof a.edges] !== to)
    .map(([k, to]) => `${k}: ${a.edges[k as keyof typeof a.edges]} -> ${to}`);
  const meta: ValueChange[] = [];
  diffValue({ name: a.name, description: a.description }, { name: b.name, description: b.description }, "", meta);

  const lines = [
    ...stepsAdded.map((s) => `+ step ${s.id} (${s.kind})`),
    ...stepsRemoved.map((s) => `- step ${s.id} (${s.kind})`),
    ...stepsChanged.map((s) => `~ step ${s.id}: ${s.changes.map((c) => c.path || "/").join(", ")}`),
    ...edgesAdded.map((e) => `+ edge ${e}`),
    ...edgesRemoved.map((e) => `- edge ${e}`),
    ...edgesRetargeted.map((e) => `~ edge ${e}`),
    ...meta.map((m) => `~ ${m.path.slice(1)}: ${JSON.stringify(m.before)} -> ${JSON.stringify(m.after)}`),
  ];
  return {
    identical: lines.length === 0,
    from: { id: a.id, revision: a.revision }, to: { id: b.id, revision: b.revision },
    stepsAdded, stepsRemoved, stepsChanged, edgesAdded, edgesRemoved, edgesRetargeted, metadata: meta,
    summary: lines.length ? lines.join("\n") : "no differences",
  };
}
