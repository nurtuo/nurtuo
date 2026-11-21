// SPDX-License-Identifier: AGPL-3.0-only
// MCP resources: flows are readable as resources so clients can attach them to a conversation.
//   nurtuo://flows/{flowId}           application/json  the stored flow document
//   nurtuo://flows/{flowId}/summary   text/plain        semantic view (steps, ports, edges)
//   nurtuo://catalog/step-kinds       application/json  step kinds
//   nurtuo://catalog/connectors       application/json  native connector catalog
import { McpServer, ResourceTemplate } from "@modelcontextprotocol/sdk/server/mcp.js";
import { McpError, ErrorCode } from "@modelcontextprotocol/sdk/types.js";
import { CONNECTORS, listStepKinds } from "./catalog.js";
import { byteSize, redact, type Limits } from "./guardrails.js";
import type { FlowStore } from "./store.js";
import { flowView } from "./view.js";

export const FLOW_URI = (id: string) => `nurtuo://flows/${encodeURIComponent(id)}`;

const one = (v: string | string[] | undefined): string => decodeURIComponent(Array.isArray(v) ? (v[0] ?? "") : (v ?? ""));

export function registerResources(server: McpServer, store: FlowStore, limits: Limits): void {
  const json = (uri: URL, value: unknown) => {
    const text = JSON.stringify(redact(value), null, 2);
    if (byteSize(text) > limits.maxOutputBytes) throw new McpError(ErrorCode.InvalidRequest, `Resource ${uri.href} exceeds ${limits.maxOutputBytes} bytes`);
    return { contents: [{ uri: uri.href, mimeType: "application/json", text }] };
  };

  const loadFlow = async (uri: URL, v: string | string[] | undefined) => {
    const id = one(v);
    const flow = await store.getFlow(id);
    if (!flow) throw new McpError(ErrorCode.InvalidParams, `Flow ${id} not found`);
    return flow;
  };

  server.registerResource("flow", new ResourceTemplate("nurtuo://flows/{flowId}", {
    list: async () => ({
      resources: (await store.listFlows()).map((f) => ({
        uri: FLOW_URI(f.id), name: f.id, title: f.name, mimeType: "application/json",
        description: `${f.name} (revision ${f.revision}, ${f.steps.length} steps)`,
      })),
    }),
    complete: { flowId: async (prefix) => (await store.listFlows()).map((f) => f.id).filter((id) => id.startsWith(prefix)) },
  }), { title: "Flow document", description: "A stored flow in nurtuo.flow/v1 format", mimeType: "application/json" },
  async (uri, { flowId }) => json(uri, await loadFlow(uri, flowId)));

  server.registerResource("flow-summary", new ResourceTemplate("nurtuo://flows/{flowId}/summary", { list: undefined }),
    { title: "Flow summary", description: "Readable summary: steps, ports and where each port leads", mimeType: "text/plain" },
    async (uri, { flowId }) => {
      const view = flowView(await loadFlow(uri, flowId));
      const text = redact(`${view.name} (${view.id}, revision ${view.revision})\n${view.summary}`);
      return { contents: [{ uri: uri.href, mimeType: "text/plain", text }] };
    });

  server.registerResource("step-kinds", "nurtuo://catalog/step-kinds",
    { title: "Step kinds", description: "All step kinds and their ports", mimeType: "application/json" },
    async (uri) => json(uri, { kinds: listStepKinds() }));

  server.registerResource("connectors", "nurtuo://catalog/connectors",
    { title: "Connector catalog", description: "Native connectors and their operations", mimeType: "application/json" },
    async (uri) => json(uri, { connectors: CONNECTORS.map((c) => ({ ...c, connectionSchema: undefined })) }));
}
