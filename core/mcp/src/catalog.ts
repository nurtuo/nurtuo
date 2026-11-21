// SPDX-License-Identifier: AGPL-3.0-only
// Static knowledge the agent needs to build flows: step kinds (with per-kind config JSON Schema
// taken from flow-schema, so it cannot drift), connectors, and explanations of validator codes.
import { flowSchema, STEP_KINDS, type Step, type StepKind } from "@nurtuo/flow-schema";
import { portsOf } from "@nurtuo/flow-validate";
import { CONNECTORS, type ConnectorInfo } from "./connectors.generated.js";

export { CONNECTORS, type ConnectorInfo };

type Json = Record<string, unknown>;

interface KindMeta {
  summary: string;
  /** Human description of the output ports. */
  ports: string;
  terminal?: boolean;
  example: Json;
  /** Typical position in a flow, used by suggest_next_steps. */
  role: "trigger" | "action" | "transform" | "control" | "terminal";
}

const META: Record<StepKind, KindMeta> = {
  start: { summary: "Trigger. Exactly one per flow; no incoming edges.", ports: "default", role: "trigger",
    example: { type: "listener", connector: "http", path: "/orders", method: "POST" } },
  connector: { summary: "Operation against an external system via a native connector (see list_connectors).", ports: "default", role: "action",
    example: { connector: "postgres", operation: "QUERY", objectType: "employee" } },
  queue: { summary: "Publish the document to a queue component.", ports: "default", role: "action",
    example: { queue: "orders", operation: "publish" } },
  map: { summary: "Transform the document with a JSONata expression.", ports: "default", role: "transform",
    example: { jsonata: "{ 'id': id, 'name': first_name & ' ' & last_name }" } },
  script: { summary: "Run a sandboxed WebAssembly module over every document.", ports: "default", role: "transform",
    example: { script: "normalize-names", timeoutMs: 2000, memoryMiB: 16 } },
  message: { summary: "Set the document body from a template.", ports: "default", role: "transform",
    example: { template: "{\"ok\": true}" } },
  setProperties: { summary: "Set document or process properties from CEL expressions.", ports: "default", role: "transform",
    example: { properties: [{ name: "orderId", scope: "document", value: "doc.id" }] } },
  decision: { summary: "Boolean CEL condition; routes to `true` or `false`.", ports: "true, false", role: "control",
    example: { cel: "doc.id != ''" } },
  branch: { summary: "Runs N paths sequentially; ports are '1'..'N'.", ports: "'1'..'<paths>'", role: "control",
    example: { paths: 2 } },
  route: { summary: "Sends the document to the first matching route; ports are each route id plus `default`.", ports: "<route ids>, default", role: "control",
    example: { routes: [{ id: "big", cel: "doc.amount > 1000" }] } },
  tryCatch: { summary: "Run the `try` path; on failure (after retries) continue on `catch`.", ports: "try, catch", role: "control",
    example: { retries: 2 } },
  processCall: { summary: "Call another flow by id.", ports: "default", role: "action",
    example: { flow: "other-flow" } },
  notify: { summary: "Send a notification (alert channel).", ports: "default", role: "action",
    example: { channel: "ops", template: "Flow failed" } },
  stop: { summary: "End the run with an optional status/message. Terminal: no ports.", ports: "none", terminal: true, role: "terminal",
    example: { status: 404, message: "not found" } },
  returnDocuments: { summary: "Return the current documents to the caller. Terminal: no ports.", ports: "none", terminal: true, role: "terminal",
    example: {} },
};

export const kindRole = (k: StepKind) => META[k].role;
export const kindExample = (k: StepKind): Json => structuredClone(META[k].example);

/** `$defs` entry holding the config JSON Schema of a kind (`mapConfig`, `stopConfig`, ...). */
export function configSchemaOf(kind: StepKind): Json | undefined {
  const defs = (flowSchema as { $defs?: Record<string, Json> }).$defs ?? {};
  const s = defs[`${kind}Config`];
  return s ? structuredClone(s) : undefined;
}

export interface StepKindInfo {
  kind: StepKind;
  summary: string;
  ports: string;
  terminal: boolean;
  /** Ports of the example config, as portsOf() computes them. */
  examplePorts: string[];
}

export function listStepKinds(): StepKindInfo[] {
  return STEP_KINDS.map((kind) => {
    const m = META[kind];
    const step: Step = { id: "x", kind, config: m.example };
    return { kind, summary: m.summary, ports: m.ports, terminal: m.terminal === true, examplePorts: portsOf(step) };
  });
}

export function getStepSchema(kind: StepKind) {
  const m = META[kind];
  return {
    kind,
    summary: m.summary,
    ports: m.ports,
    terminal: m.terminal === true,
    configSchema: configSchemaOf(kind) ?? null,
    exampleStep: { id: `${kind.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`)}_1`, kind, config: m.example },
    idPattern: "^[a-z][a-z0-9_-]*$",
    notes: kind === "connector"
      ? "Use list_connectors for valid `connector` and `operation` values. Never put credentials in config; `connection` names a connection configured in the control plane."
      : kind === "map" ? "JSONata over the current document." : kind === "decision" || kind === "route" ? "CEL expressions; `doc` is the current document." : undefined,
  };
}

// ---- Diagnostics ----

export interface DiagnosticHelp {
  code: string;
  title: string;
  explanation: string;
  fix: string;
}

const HELP: DiagnosticHelp[] = [
  { code: "step.duplicate_id", title: "Duplicate step id", explanation: "Two steps share an id. Ids address steps in edges and ops, so they must be unique.", fix: "renameStep one of them, or removeStep the duplicate." },
  { code: "flow.start_count", title: "Wrong number of start steps", explanation: "A flow needs exactly one start step.", fix: "addStep a `start` step if there is none; removeStep extra start steps if there are several." },
  { code: "edge.source_unknown", title: "Edge from a missing step", explanation: "An edge key refers to a step id that does not exist (often after removing or renaming a step).", fix: "disconnect the stale edge or re-create the missing step." },
  { code: "edge.port_invalid", title: "Edge from a port the step does not have", explanation: "The edge key `step:port` uses a port that the step kind does not expose (e.g. `default` on a decision, which has `true`/`false`; or a branch/route whose config changed).", fix: "disconnect the invalid edge and connect from a valid port. See get_step_schema for the ports of the kind." },
  { code: "edge.target_unknown", title: "Edge to a missing step", explanation: "An edge points at a step id that does not exist.", fix: "Re-create the target step with addStep, or connect the port to an existing step." },
  { code: "start.has_incoming", title: "Start step has an incoming edge", explanation: "Triggers cannot be the target of an edge.", fix: "disconnect the edge that points at the start step." },
  { code: "step.port_unconnected", title: "Output port not connected", explanation: "Every output port must lead somewhere. Terminal kinds (`returnDocuments`, `stop`) have no ports.", fix: "addStep with `after: {step, port}` to append a step on that port, or connect it to an existing step. Use suggest_next_steps for candidates." },
  { code: "route.reserved_id", title: "Route id `default` is reserved", explanation: "The `default` port of a route step always exists; routes cannot reuse the id.", fix: "updateConfig with a routes array using another id (arrays replace), then fix the edges." },
  { code: "route.duplicate_id", title: "Duplicate route id", explanation: "Two routes of one step share an id, so two ports would collide.", fix: "updateConfig with unique route ids." },
  { code: "setProperties.duplicate_name", title: "Property set twice", explanation: "The same property name appears twice in the same scope.", fix: "updateConfig with a de-duplicated properties array." },
  { code: "processCall.self_reference", title: "Flow calls itself", explanation: "processCall.flow equals the flow's own id; this would recurse forever.", fix: "updateConfig to point at a different flow, or remove the step." },
  { code: "flow.cycle", title: "Cycle in the flow", explanation: "The M1 engine interprets a DAG. The step is reachable from itself.", fix: "disconnect one edge on the cycle and route that port to a terminal step instead." },
  { code: "step.unreachable", title: "Step not reachable from start (warning)", explanation: "No path from the start step leads here, so it never runs. This is a warning, not an error.", fix: "connect an open port to it, or removeStep it." },
  { code: "schema.required", title: "Required field missing", explanation: "The document (or a step config) lacks a required property. The path says where.", fix: "updateConfig (merge patch) to add the field; get_step_schema shows required fields per kind." },
  { code: "schema.additionalProperties", title: "Unknown property", explanation: "Configs and steps are closed objects; an unknown key was supplied.", fix: "updateConfig with the key set to null to delete it. Check the name against get_step_schema." },
  { code: "schema.enum", title: "Value not allowed", explanation: "The value is not one of the permitted options.", fix: "Use one of the values listed in get_step_schema / list_connectors." },
  { code: "schema.type", title: "Wrong type", explanation: "The value has the wrong JSON type (e.g. string instead of integer).", fix: "updateConfig with a value of the right type." },
  { code: "schema.const", title: "Constant mismatch", explanation: "A fixed value was expected (e.g. schema version `nurtuo.flow/v1`).", fix: "Use the expected value." },
  { code: "schema.minLength", title: "Value too short", explanation: "An empty string was given where text is required.", fix: "Provide a non-empty value." },
  { code: "schema.pattern", title: "Value has the wrong format", explanation: "The string does not match the required pattern (step ids: ^[a-z][a-z0-9_-]*$; route ids: ^[A-Za-z0-9_-]+$).", fix: "renameStep / updateConfig with a conforming value." },
  { code: "schema.minimum", title: "Number too small", explanation: "The number is below the allowed minimum (e.g. branch paths >= 2).", fix: "updateConfig with an in-range number." },
  { code: "schema.maximum", title: "Number too large", explanation: "The number is above the allowed maximum.", fix: "updateConfig with an in-range number." },
  { code: "schema.minItems", title: "Array too short", explanation: "A list needs at least one entry (e.g. route.routes, setProperties.properties).", fix: "updateConfig with a non-empty array." },
];

export function explainCode(code: string): DiagnosticHelp | undefined {
  return HELP.find((h) => h.code === code);
}
export const KNOWN_DIAGNOSTIC_CODES = HELP.map((h) => h.code);
