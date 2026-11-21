// SPDX-License-Identifier: AGPL-3.0-only
// MCP prompts: guided workflows a user can pick from the client UI.
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { z } from "zod";

const user = (text: string) => ({ messages: [{ role: "user" as const, content: { type: "text" as const, text } }] });

export const PROMPTS = ["build-integration", "fix-flow", "explain-flow"] as const;

export function buildIntegrationPrompt(a: { goal: string; source?: string | undefined; target?: string | undefined; flowId?: string | undefined }): string {
  return [
    `Build a Nurtuo integration flow for this goal: ${a.goal}`,
    a.source ? `Source system: ${a.source}` : "",
    a.target ? `Target system: ${a.target}` : "",
    a.flowId ? `Use flow id "${a.flowId}".` : "Choose a short kebab-case flow id.",
    "",
    "Work in this order and use the tools, do not guess:",
    "1. list_connectors to see which connectors and operations exist; list_step_kinds (and get_step_schema for any kind you use) for step configs.",
    "2. search for existing flows or components you can reuse (processCall).",
    "3. apply_ops with `create` to build the flow: one `start` step, then addStep with `after` to chain steps, `decision`/`tryCatch` where the goal needs branching or error handling. Every output port must lead somewhere and every path must end in `returnDocuments` or `stop`.",
    "4. Read the diagnostics in the apply_ops result. While valid is false, use suggest_next_steps and explain_diagnostic, then send corrective ops. Do not remove steps just to silence a diagnostic.",
    "5. When valid is true, call dry_run with realistic sample input (if a runtime is available) and report the trace.",
    "6. Finish with a short summary of the flow: steps in order, the connector operations used, what the human must configure (connections, credentials in the control plane).",
    "",
    "Rules: never put passwords, tokens or keys into a flow; reference connections by name. Do not deploy unless the human explicitly asks; deployment needs their approval.",
  ].filter((l, i, arr) => l !== "" || arr[i - 1] !== "").join("\n");
}

export function registerPrompts(server: McpServer): void {
  server.registerPrompt("build-integration", {
    title: "Build an integration",
    description: "Guided workflow: design, build, validate and dry-run a flow for a stated goal.",
    argsSchema: {
      goal: z.string().describe("What the integration should do, in plain language"),
      source: z.string().optional().describe("Source system, e.g. 'HTTP API' or 'Postgres'"),
      target: z.string().optional().describe("Target system, e.g. 'SFTP server'"),
      flowId: z.string().optional().describe("Flow id to create"),
    },
  }, (a) => user(buildIntegrationPrompt(a)));

  server.registerPrompt("fix-flow", {
    title: "Fix a flow",
    description: "Validate a flow and repair it until it is valid.",
    argsSchema: { flowId: z.string().describe("Flow to repair") },
  }, ({ flowId }) => user([
    `Repair the flow "${flowId}".`,
    "1. get_flow, then validate.",
    "2. For each error diagnostic call explain_diagnostic (with flowId and stepId) and apply the smallest corrective ops with apply_ops.",
    "3. Use suggest_next_steps for open ports. Repeat until valid is true.",
    "4. Summarise what you changed. Do not delete steps unless they are clearly redundant.",
  ].join("\n")));

  server.registerPrompt("explain-flow", {
    title: "Explain a flow",
    description: "Describe what a flow does in plain language.",
    argsSchema: { flowId: z.string().describe("Flow to explain") },
  }, ({ flowId }) => user([
    `Explain the flow "${flowId}" to a non-technical colleague.`,
    "Use get_flow; walk the steps from the start along each port. Mention the trigger, the systems touched (connector steps), transformations, decisions and how errors end. Note any diagnostics from validate.",
  ].join("\n")));
}
