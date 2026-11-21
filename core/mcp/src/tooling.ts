// SPDX-License-Identifier: AGPL-3.0-only
// Tool registration wrapper applying the guardrails uniformly: input size limit, per-call timeout,
// structured errors, output redaction and output size limit.
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { ZodRawShape } from "zod";
import { ToolError, type ErrorCode } from "./errors.js";
import { byteSize, checkInputSize, redact, withTimeout, type Limits } from "./guardrails.js";
import { RevisionConflictError } from "./store.js";

export interface Reply { value: unknown; isError: boolean }
export const ok = (value: unknown): Reply => ({ value, isError: false });
/** Domain failure with a custom body (kept for backwards-compatible shapes). */
export const failBody = (value: unknown): Reply => ({ value, isError: true });
export const fail = (code: ErrorCode, message: string, extra: Record<string, unknown> = {}): Reply =>
  ({ value: { error: message, code, ...extra }, isError: true });

export interface ToolConfig {
  title?: string;
  description: string;
  inputSchema: ZodRawShape;
  readOnly?: boolean;
  destructive?: boolean;
  idempotent?: boolean;
  /** Tool talks to systems outside this server. */
  openWorld?: boolean;
}

const asText = (r: Reply) => ({
  content: [{ type: "text" as const, text: JSON.stringify(r.value, null, 2) }],
  ...(r.isError ? { isError: true } : {}),
});

export function makeRegistrar(server: McpServer, limits: Limits) {
  return function tool(name: string, cfg: ToolConfig, handler: (args: any) => Promise<Reply> | Reply): void { // eslint-disable-line @typescript-eslint/no-explicit-any
    const config = {
      ...(cfg.title ? { title: cfg.title } : {}),
      description: cfg.description,
      inputSchema: cfg.inputSchema,
      annotations: {
        readOnlyHint: cfg.readOnly === true,
        destructiveHint: cfg.destructive === true,
        idempotentHint: cfg.idempotent ?? cfg.readOnly === true,
        openWorldHint: cfg.openWorld === true,
      },
    };
    const cb = async (args: unknown) => {
      let reply: Reply;
      try {
        checkInputSize(args, limits);
        reply = await withTimeout(Promise.resolve(handler(args)), limits.callTimeoutMs, `Tool ${name}`);
      } catch (e) {
        reply = errorReply(e);
      }
      const safe: Reply = { value: redact(reply.value), isError: reply.isError };
      const size = byteSize(safe.value);
      if (size > limits.maxOutputBytes) {
        return asText(fail("limit_exceeded", `Result is ${size} bytes; the limit is ${limits.maxOutputBytes}. Narrow the request.`, { limit: "maxOutputBytes", max: limits.maxOutputBytes, actual: size }));
      }
      return asText(safe);
    };
    (server.registerTool as (n: string, c: unknown, cb: unknown) => unknown).call(server, name, config, cb);
  };
}

export function errorReply(e: unknown): Reply {
  if (e instanceof ToolError) return fail(e.code, e.message, e.details ?? {});
  if (e instanceof RevisionConflictError) {
    return failBody({ applied: false, error: e.message, code: "revision_conflict", expected: e.expected, actual: e.actual, message: e.message });
  }
  // Never leak internals or stack traces to the model; the message of unexpected errors is redacted by the caller.
  return fail("internal", e instanceof Error ? e.message : "unexpected error");
}
