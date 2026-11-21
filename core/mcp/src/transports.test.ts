// SPDX-License-Identifier: AGPL-3.0-only
import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { createServer, depsFromConfig, InMemoryFlowStore, parseCli, startHttp, UsageError } from "./index.js";

const text = (r: unknown) => ((r as { content: { text: string }[] }).content[0] as { text: string }).text;

test("parseCli: defaults, control plane, env fallbacks, errors", () => {
  const d = parseCli([], {});
  assert.deepEqual([d.store, d.transport, d.port, d.host, d.allowDeploy], ["memory", "stdio", 3333, "127.0.0.1", false]);

  const cp = parseCli(["--url", "http://cp:8080", "--token", "t", "--allow-deploy", "--max-ops", "10", "--timeout-ms", "500"], {});
  assert.deepEqual([cp.store, cp.url, cp.token, cp.allowDeploy, cp.maxOps, cp.timeoutMs], ["control-plane", "http://cp:8080", "t", true, 10, 500]);

  const env = parseCli(["--store", "control-plane"], { NURTUO_URL: "http://e", NURTUO_TOKEN: "et" });
  assert.deepEqual([env.url, env.token], ["http://e", "et"]);

  assert.throws(() => parseCli(["--store", "control-plane"], {}), UsageError);
  assert.throws(() => parseCli(["--store", "redis"], {}), /memory or control-plane/);
  assert.throws(() => parseCli(["--transport", "ws"], {}), /stdio or http/);
  assert.throws(() => parseCli(["--port", "abc"], {}), /positive integer/);
  assert.throws(() => parseCli(["--bogus"], {}), UsageError);
  assert.throws(() => parseCli(["--transport", "http", "--host", "0.0.0.0"], {}), /auth-token/);
  assert.equal(parseCli(["--transport", "http", "--host", "0.0.0.0", "--auth-token", "x"], {}).authToken, "x");
  assert.equal(parseCli(["-h"], {}).help, true);
});

test("depsFromConfig wires deploy only on request", () => {
  const mem = depsFromConfig(parseCli([], {}));
  assert.equal(mem.allowDeploy, false);
  assert.ok(mem.deployer);
  const cp = depsFromConfig(parseCli(["--url", "http://localhost:1", "--allow-deploy", "--max-ops", "7"], {}));
  assert.equal(cp.allowDeploy, true);
  assert.equal(cp.limits?.maxOps, 7);
});

test("streamable HTTP transport serves tools, enforces bearer auth and origin", async () => {
  const store = new InMemoryFlowStore();
  const h = await startHttp({ createServer: () => createServer({ store }), port: 0, authToken: "s3cret" });
  try {
    const noAuth = await fetch(h.url, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream" }, body: "{}" });
    assert.equal(noAuth.status, 401);
    const badOrigin = await fetch(h.url, { method: "POST", headers: { origin: "https://evil.example", authorization: "Bearer s3cret" }, body: "{}" });
    assert.equal(badOrigin.status, 403);
    assert.equal((await fetch(h.url, { method: "GET", headers: { authorization: "Bearer s3cret" } })).status, 405);
    assert.equal((await fetch(h.url.replace("/mcp", "/nope"))).status, 404);
    assert.equal((await fetch(h.url.replace("/mcp", "/healthz"))).status, 200);

    const client = new Client({ name: "t", version: "0" });
    await client.connect(new StreamableHTTPClientTransport(new URL(h.url), { requestInit: { headers: { authorization: "Bearer s3cret" } } }));
    const tools = (await client.listTools()).tools.map((t) => t.name);
    assert.ok(tools.includes("apply_ops") && tools.includes("list_connectors"));
    const r = await client.callTool({ name: "apply_ops", arguments: {
      flowId: "h", create: { name: "H" }, ops: [{ op: "addStep", step: { id: "start", kind: "start", config: { type: "manual" } } }],
    } });
    assert.equal(JSON.parse(text(r)).applied, true);
    // Stateless server: state lives in the shared store, visible on the next request.
    assert.equal((await store.getFlow("h"))?.steps.length, 1);
    assert.equal(JSON.parse(text(await client.callTool({ name: "get_flow", arguments: { flowId: "h" } }))).id, "h");
    await client.close();
  } finally {
    await h.close();
  }
});

test("HTTP transport rejects oversized and malformed bodies", async () => {
  const h = await startHttp({ createServer: () => createServer({ store: new InMemoryFlowStore() }), port: 0, maxBodyBytes: 100 });
  try {
    const big = await fetch(h.url, { method: "POST", body: JSON.stringify({ pad: "x".repeat(500) }) });
    assert.equal(big.status, 413);
    const bad = await fetch(h.url, { method: "POST", body: "{nope" });
    assert.equal(bad.status, 400);
  } finally {
    await h.close();
  }
});

test("stdio entrypoint (bin) runs end to end", async () => {
  const entry = fileURLToPath(new URL("./stdio.js", import.meta.url));
  const transport = new StdioClientTransport({ command: process.execPath, args: [entry, "--allow-deploy"], stderr: "pipe" });
  const client = new Client({ name: "t", version: "0" });
  await client.connect(transport);
  try {
    const tools = (await client.listTools()).tools.map((t) => t.name);
    assert.ok(tools.includes("deploy"), "memory store + --allow-deploy exposes deploy");
    const r = JSON.parse(text(await client.callTool({ name: "list_connectors", arguments: {} })));
    assert.equal(r.connectors.length, 4);
  } finally {
    await client.close();
  }
});

test("stdio entrypoint prints usage and exits 2 on bad flags", async () => {
  const { spawnSync } = await import("node:child_process");
  const entry = fileURLToPath(new URL("./stdio.js", import.meta.url));
  const r = spawnSync(process.execPath, [entry, "--store", "bogus"], { encoding: "utf8" });
  assert.equal(r.status, 2);
  assert.match(r.stderr, /Usage: nurtuo-mcp/);
  assert.equal(r.stdout, "", "stdout stays clean");
  const help = spawnSync(process.execPath, [entry, "--help"], { encoding: "utf8" });
  assert.equal(help.status, 0);
  assert.match(help.stdout, /--allow-deploy/);
});
