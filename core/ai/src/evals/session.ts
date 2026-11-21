// SPDX-License-Identifier: AGPL-3.0-only
// An eval session: an MCP client connected in-process to a fresh Nurtuo MCP server. Agents (scripted
// or LLM-driven) act only through `call`, exactly like a real MCP client, and every call is recorded.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { createServer, InMemoryFlowStore, type DryRunner, type FlowStore, type ServerDeps } from "@nurtuo/mcp";

export interface ToolCallRecord {
  name: string;
  args: unknown;
  isError: boolean;
  result: unknown;
}

/** Tool description in the shape LLM tool-calling APIs expect (name, description, JSON Schema). */
export interface ToolSpec {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

export interface SessionOptions {
  store?: FlowStore;
  runtime?: DryRunner;
  deps?: Partial<ServerDeps>;
}

export class EvalSession {
  readonly transcript: ToolCallRecord[] = [];
  private constructor(readonly store: FlowStore, readonly client: Client, private readonly closeAll: () => Promise<void>) {}

  static async open(opts: SessionOptions = {}): Promise<EvalSession> {
    const store = opts.store ?? new InMemoryFlowStore();
    const server = createServer({ store, ...(opts.runtime ? { runtime: opts.runtime } : {}), ...opts.deps });
    const [ct, st] = InMemoryTransport.createLinkedPair();
    const client = new Client({ name: "nurtuo-eval", version: "0.0.0" });
    await Promise.all([server.connect(st), client.connect(ct)]);
    return new EvalSession(store, client, async () => { await client.close(); await server.close(); });
  }

  /** Calls a tool and returns its parsed JSON body. Never throws on tool errors; check `isError`. */
  async call(name: string, args: Record<string, unknown> = {}): Promise<{ isError: boolean; body: any }> { // eslint-disable-line @typescript-eslint/no-explicit-any
    const r = await this.client.callTool({ name, arguments: args });
    const text = (r.content as { type: string; text?: string }[]).find((c) => c.type === "text")?.text ?? "";
    let body: unknown;
    try { body = JSON.parse(text); } catch { body = { raw: text }; }
    const isError = r.isError === true;
    this.transcript.push({ name, args, isError, result: body });
    return { isError, body };
  }

  /** The tools an LLM would be offered. */
  async tools(): Promise<ToolSpec[]> {
    const { tools } = await this.client.listTools();
    return tools.map((t) => ({ name: t.name, description: t.description ?? "", inputSchema: t.inputSchema as Record<string, unknown> }));
  }

  async prompt(name: string, args: Record<string, string>): Promise<string> {
    const r = await this.client.getPrompt({ name, arguments: args });
    return r.messages.map((m) => (m.content as { text?: string }).text ?? "").join("\n");
  }

  get toolCalls(): number {
    return this.transcript.length;
  }

  close(): Promise<void> {
    return this.closeAll();
  }
}
