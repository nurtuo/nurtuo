// SPDX-License-Identifier: AGPL-3.0-only
// Test helper: an MCP client wired to a server over the in-memory transport.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { createServer, type ServerDeps } from "./server.js";

export interface Connected {
  client: Client;
  call(name: string, args?: Record<string, unknown>): Promise<{ isError: boolean; body: any }>; // eslint-disable-line @typescript-eslint/no-explicit-any
  close(): Promise<void>;
}

export async function connectClient(deps: ServerDeps): Promise<Connected> {
  const server = createServer(deps);
  const [ct, st] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: "test", version: "0.0.0" });
  await Promise.all([server.connect(st), client.connect(ct)]);
  return {
    client,
    async call(name, args = {}) {
      const r = await client.callTool({ name, arguments: args });
      const text = (r.content as { text: string }[])[0]?.text ?? "";
      let body: unknown;
      try { body = JSON.parse(text); } catch { body = { raw: text }; }
      return { isError: r.isError === true, body };
    },
    async close() { await client.close(); await server.close(); },
  };
}
