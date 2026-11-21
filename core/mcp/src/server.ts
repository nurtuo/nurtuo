// SPDX-License-Identifier: AGPL-3.0-only
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { z } from "zod";
import { applyOps, FLOW_SCHEMA_VERSION, STEP_KINDS, type Flow, type FlowCommand } from "@nurtuo/flow-schema";
import { hasErrors, validateFlow } from "@nurtuo/flow-validate";
import { RevisionConflictError, type Deployer, type FlowStore } from "./store.js";
import type { DryRunner } from "./runtime.js";
import { flowView } from "./view.js";
import { CONNECTORS, getStepSchema, listStepKinds } from "./catalog.js";
import { diffFlows, explainDiagnostic, suggestNextSteps } from "./assist.js";
import { DEFAULT_LIMITS, findLiteralSecrets, type Limits } from "./guardrails.js";
import { fail, failBody, makeRegistrar, ok } from "./tooling.js";
import { registerResources } from "./resources.js";
import { registerPrompts } from "./prompts.js";

export interface ServerDeps {
  store: FlowStore;
  /** Optional: without a runtime, `dry_run` reports that it is unavailable. */
  runtime?: DryRunner;
  /** Optional: enables `list_runtimes` and `deployment_status`. */
  deployer?: Deployer;
  /** Registers the `deploy` tool (needs `deployer`). Off by default: agents read and write flows only. */
  allowDeploy?: boolean;
  /** Overrides for the guardrail limits. */
  limits?: Partial<Limits>;
}

const stepSchema = z.object({
  id: z.string().max(128),
  kind: z.enum(STEP_KINDS),
  label: z.string().max(512).optional(),
  config: z.record(z.string(), z.unknown()),
});
const opSchema = z.discriminatedUnion("op", [
  z.object({ op: z.literal("addStep"), step: stepSchema, after: z.object({ step: z.string(), port: z.string() }).optional() }),
  z.object({ op: z.literal("removeStep"), id: z.string() }),
  z.object({ op: z.literal("updateConfig"), id: z.string(), patch: z.record(z.string(), z.unknown()) }),
  z.object({ op: z.literal("connect"), from: z.string(), port: z.string(), to: z.string() }),
  z.object({ op: z.literal("disconnect"), from: z.string(), port: z.string() }),
  z.object({ op: z.literal("setLabel"), id: z.string(), label: z.string().nullable() }),
  z.object({ op: z.literal("renameStep"), id: z.string(), newId: z.string() }),
]);

const flowRef = z.object({
  flowId: z.string().optional().describe("Stored flow id"),
  revision: z.number().int().min(1).optional().describe("Revision number; default: latest"),
  flow: z.record(z.string(), z.unknown()).optional().describe("Inline flow document instead of flowId"),
});

export const MCP_TOOLS_V1 = ["search", "get_flow", "apply_ops", "validate", "dry_run"] as const;
export const MCP_TOOLS_V2 = [
  "list_step_kinds", "get_step_schema", "list_connectors", "suggest_next_steps", "explain_diagnostic", "diff_flows",
] as const;
export const MCP_TOOLS_DEPLOY = ["list_runtimes", "deployment_status", "deploy"] as const;

export function createServer(deps: ServerDeps): McpServer {
  const { store, runtime, deployer } = deps;
  const limits: Limits = { ...DEFAULT_LIMITS, ...deps.limits };
  const server = new McpServer(
    { name: "nurtuo", version: "0.1.0" },
    { instructions: "Nurtuo integration flows. Start with list_connectors and list_step_kinds, build with apply_ops, then validate and dry_run. Never put credentials in flows; reference connections by name." },
  );
  const tool = makeRegistrar(server, limits);

  const loadFlow = async (flowId: string) => {
    const flow = await store.getFlow(flowId);
    return flow ?? undefined;
  };
  const notFound = (flowId: string) => fail("not_found", `Flow ${flowId} not found`, { flowId });

  // ------------------------------------------------------------------ v1 tools

  tool("search", {
    description: "Search components and flows by text (case-insensitive substring over id, name/description, kind).",
    inputSchema: { query: z.string().max(500), type: z.enum(["all", "component", "flow"]).default("all") },
    readOnly: true,
  }, async ({ query, type }) => {
    const q = query.toLowerCase();
    const hit = (...fields: (string | undefined)[]) => fields.some((f) => f?.toLowerCase().includes(q));
    const components = type === "flow" ? [] : (await store.listComponents()).filter((c) => hit(c.id, c.kind, c.description));
    const flows = type === "component" ? [] : (await store.listFlows())
      .filter((f) => hit(f.id, f.name, f.description))
      .map((f) => ({ id: f.id, name: f.name, revision: f.revision }));
    return ok({ components, flows });
  });

  tool("get_flow", {
    description: "Get a flow in a semantic view: steps with ports and where each port leads, plus edge lines.",
    inputSchema: { flowId: z.string() },
    readOnly: true,
  }, async ({ flowId }) => {
    const flow = await loadFlow(flowId);
    return flow ? ok(flowView(flow)) : notFound(flowId);
  });

  tool("apply_ops", {
    description:
      "Apply CommandBus ops to a flow atomically (all or none). With `create`, a missing flow is created empty first. " +
      "The result is stored even if the flow is not yet valid; diagnostics describe what is left to fix. " +
      "Literal credentials in ops are refused: reference connections by name.",
    inputSchema: {
      flowId: z.string().max(200),
      ops: z.array(opSchema).min(1),
      create: z.object({ name: z.string().max(200), description: z.string().max(2000).optional() }).optional(),
    },
    idempotent: false,
  }, async ({ flowId, ops, create }) => {
    if (ops.length > limits.maxOps) {
      return fail("limit_exceeded", `${ops.length} ops in one call; the limit is ${limits.maxOps}. Send several smaller apply_ops calls.`, { limit: "maxOps", max: limits.maxOps, actual: ops.length });
    }
    const secrets = findLiteralSecrets(ops, "/ops");
    if (secrets.length) {
      return fail("secret_in_input", "Ops contain what looks like a literal credential. Flows never store secrets; configure a connection in the control plane and reference it by name.", { paths: secrets });
    }
    let base = await loadFlow(flowId);
    if (!base) {
      if (!create) return fail("not_found", `Flow ${flowId} not found; pass \`create\` to create it`, { flowId });
      base = {
        schema: FLOW_SCHEMA_VERSION, id: flowId, name: create.name,
        ...(create.description !== undefined ? { description: create.description } : {}),
        revision: 0, steps: [], edges: {},
      } as Flow;
    }
    const r = applyOps(base, ops as FlowCommand[]);
    if (!r.ok) {
      return failBody({ applied: false, failedOpIndex: r.index, code: r.code, error: r.message, message: r.message });
    }
    if (r.flow.steps.length > limits.maxSteps) {
      return fail("limit_exceeded", `Flow would have ${r.flow.steps.length} steps; the limit is ${limits.maxSteps}. Split it with processCall.`, { limit: "maxSteps", max: limits.maxSteps });
    }
    const next: Flow = { ...r.flow, revision: base.revision + 1 };
    try {
      await store.saveFlow(next, base.revision);
    } catch (e) {
      if (e instanceof RevisionConflictError) {
        return failBody({ applied: false, code: "revision_conflict", error: e.message, message: e.message, expected: e.expected, actual: e.actual, hint: "Re-read the flow with get_flow and re-apply your ops." });
      }
      throw e;
    }
    const diagnostics = validateFlow(next);
    return ok({ applied: true, revision: next.revision, valid: !hasErrors(diagnostics), diagnostics, flow: flowView(next) });
  });

  tool("validate", {
    description: "Validate a stored flow with the shared flow validator; returns diagnostics.",
    inputSchema: { flowId: z.string() },
    readOnly: true,
  }, async ({ flowId }) => {
    const flow = await loadFlow(flowId);
    if (!flow) return notFound(flowId);
    const diagnostics = validateFlow(flow);
    return ok({ valid: !hasErrors(diagnostics), diagnostics });
  });

  tool("dry_run", {
    description: "Dry-run a valid flow with sample input via the runtime; no side effects. Refuses invalid flows.",
    inputSchema: { flowId: z.string(), input: z.unknown().optional() },
    readOnly: true,
  }, async ({ flowId, input }) => {
    const flow = await loadFlow(flowId);
    if (!flow) return notFound(flowId);
    const diagnostics = validateFlow(flow);
    if (hasErrors(diagnostics)) return failBody({ ran: false, error: "flow is invalid", code: "invalid_flow", reason: "flow is invalid", diagnostics });
    if (!runtime) return failBody({ ran: false, error: "no runtime configured for dry_run", code: "unavailable", reason: "no runtime configured for dry_run" });
    return ok({ ran: true, warnings: diagnostics, result: await runtime.dryRun(flow, input ?? null) });
  });

  // ------------------------------------------------------------------ catalog tools

  tool("list_step_kinds", {
    description: "List all step kinds with a summary and their output ports. Use get_step_schema for the config JSON Schema of one kind.",
    inputSchema: {},
    readOnly: true,
  }, () => ok({ kinds: listStepKinds() }));

  tool("get_step_schema", {
    description: "Config JSON Schema, ports and an example step for one step kind.",
    inputSchema: { kind: z.enum(STEP_KINDS) },
    readOnly: true,
  }, ({ kind }) => ok(getStepSchema(kind)));

  tool("list_connectors", {
    description:
      "Catalog of the native connectors (http, nats, postgres, sftp): operations (use `operation` as config.operation of a connector step), " +
      "connection fields and options. Connection schemas describe what the control plane stores; never put those values in flows.",
    inputSchema: { id: z.string().optional().describe("Return only this connector"), includeConnectionSchema: z.boolean().default(false) },
    readOnly: true,
  }, ({ id, includeConnectionSchema }) => {
    const list = CONNECTORS.filter((c) => !id || c.id === id);
    if (id && list.length === 0) return fail("not_found", `Unknown connector ${id}. Known: ${CONNECTORS.map((c) => c.id).join(", ")}`);
    return ok({
      connectors: list.map((c) => ({
        ...c,
        connectionSchema: includeConnectionSchema ? c.connectionSchema : undefined,
        stepConfigExample: { connector: c.id, operation: c.operations[0]?.operation },
      })),
    });
  });

  tool("explain_diagnostic", {
    description: "Explain a validator diagnostic code and how to fix it with ops. Pass flowId and stepId for concrete example ops.",
    inputSchema: {
      code: z.string().max(200),
      path: z.string().max(500).optional(),
      stepId: z.string().max(200).optional(),
      flowId: z.string().optional(),
    },
    readOnly: true,
  }, async ({ code, path, stepId, flowId }) => {
    const flow = flowId ? await loadFlow(flowId) : undefined;
    if (flowId && !flow) return notFound(flowId);
    return ok(explainDiagnostic({ code, ...(path ? { path } : {}), ...(stepId ? { stepId } : {}) }, flow));
  });

  tool("suggest_next_steps", {
    description:
      "Heuristic guidance for the next ops: open (unconnected) ports with candidate steps and ready-to-send addStep ops, " +
      "plus fixes for current validation errors. Example configs are placeholders to adapt.",
    inputSchema: { flowId: z.string() },
    readOnly: true,
  }, async ({ flowId }) => {
    const flow = await loadFlow(flowId);
    return flow ? ok(suggestNextSteps(flow)) : notFound(flowId);
  });

  const resolveRef = async (ref: z.infer<typeof flowRef>, side: string): Promise<{ flow?: Flow; error?: ReturnType<typeof fail> }> => {
    if (ref.flow) {
      const f = ref.flow as Partial<Flow>;
      if (!Array.isArray(f.steps) || typeof f.edges !== "object" || f.edges === null) {
        return { error: fail("invalid_input", `${side}.flow must have steps (array) and edges (object)`) };
      }
      return { flow: { schema: FLOW_SCHEMA_VERSION, id: "inline", name: "inline", revision: 0, ...f } as Flow };
    }
    if (!ref.flowId) return { error: fail("invalid_input", `${side} needs flowId or flow`) };
    if (ref.revision !== undefined) {
      if (!store.getFlowRevision) return { error: fail("unavailable", "This store does not keep revision history") };
      const f = await store.getFlowRevision(ref.flowId, ref.revision);
      return f ? { flow: f } : { error: fail("not_found", `Flow ${ref.flowId} revision ${ref.revision} not found`) };
    }
    const f = await loadFlow(ref.flowId);
    return f ? { flow: f } : { error: notFound(ref.flowId) };
  };

  tool("diff_flows", {
    description: "Structural diff of two flows or two revisions of one flow: steps added/removed/changed (with config paths), edges added/removed/retargeted.",
    inputSchema: { left: flowRef, right: flowRef },
    readOnly: true,
  }, async ({ left, right }) => {
    const a = await resolveRef(left, "left");
    if (a.error) return a.error;
    const b = await resolveRef(right, "right");
    if (b.error) return b.error;
    return ok(diffFlows(a.flow as Flow, b.flow as Flow));
  });

  // ------------------------------------------------------------------ deployment

  if (deployer) {
    tool("list_runtimes", {
      description: "List runtimes a flow can be deployed to.",
      inputSchema: {},
      readOnly: true, openWorld: true,
    }, async () => ok({ runtimes: await deployer.listRuntimes() }));

    tool("deployment_status", {
      description: "Status of one deployment (deploymentId) or the most recent deployments, optionally filtered by flowId / runtimeId.",
      inputSchema: {
        deploymentId: z.string().optional(), flowId: z.string().optional(), runtimeId: z.string().optional(),
        limit: z.number().int().min(1).max(100).default(20),
      },
      readOnly: true, openWorld: true,
    }, async ({ deploymentId, flowId, runtimeId, limit }) => {
      if (deploymentId) {
        const d = await deployer.getDeployment(deploymentId);
        return d ? ok({ deployment: d }) : fail("not_found", `Deployment ${deploymentId} not found`);
      }
      const all = await deployer.listDeployments({ ...(flowId ? { flowId } : {}), ...(runtimeId ? { runtimeId } : {}) });
      const sorted = [...all].sort((x, y) => (y.createdAt ?? "").localeCompare(x.createdAt ?? "") || (y.generation ?? 0) - (x.generation ?? 0));
      return ok({ deployments: sorted.slice(0, limit), total: all.length });
    });

    if (deps.allowDeploy === true) {
      tool("deploy", {
        description:
          "Deploy a valid flow revision to a runtime. Changes running systems: first call without `confirm` returns the plan; " +
          "call again with confirm=true only after the human has approved it. Follow with deployment_status.",
        inputSchema: {
          flowId: z.string(), runtimeId: z.string(),
          revision: z.number().int().min(1).optional().describe("Default: latest revision"),
          confirm: z.boolean().default(false).describe("Set true only after explicit human approval"),
        },
        destructive: true, idempotent: false, openWorld: true,
      }, async ({ flowId, runtimeId, revision, confirm }) => {
        if (deps.allowDeploy !== true) return fail("deploy_disabled", "deploy is disabled; start the server with --allow-deploy");
        const latest = await loadFlow(flowId);
        if (!latest) return notFound(flowId);
        let flow = latest;
        if (revision !== undefined && revision !== latest.revision) {
          if (!store.getFlowRevision) return fail("unavailable", "This store cannot deploy a non-latest revision");
          const f = await store.getFlowRevision(flowId, revision);
          if (!f) return fail("not_found", `Flow ${flowId} revision ${revision} not found`);
          flow = f;
        }
        const diagnostics = validateFlow(flow);
        if (hasErrors(diagnostics)) {
          return failBody({ deployed: false, error: "flow is invalid", code: "invalid_flow", diagnostics });
        }
        const runtimes = await deployer.listRuntimes();
        const rt = runtimes.find((r) => r.id === runtimeId);
        if (!rt) return fail("not_found", `Runtime ${runtimeId} not found. Known: ${runtimes.map((r) => r.id).join(", ") || "none"}`);
        const plan = { flowId, revision: flow.revision, runtimeId, environment: rt.environment, steps: flow.steps.length, warnings: diagnostics };
        if (!confirm) {
          return ok({ deployed: false, requiresConfirmation: true, plan, message: "Ask the human to approve, then call deploy again with confirm=true." });
        }
        return ok({ deployed: true, plan, deployment: await deployer.deploy({ flowId, runtimeId, revision: flow.revision }) });
      });
    }
  }

  registerResources(server, store, limits);
  registerPrompts(server);
  return server;
}
