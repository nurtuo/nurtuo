// SPDX-License-Identifier: AGPL-3.0-only
import { ApiError } from "../domain.js";
import type { Endpoint } from "../auth/endpoints.js";
import type { TestRunRequest, TestRunner } from "./runner.js";

/** POST /v1/workspaces/{ws}/test-runs. The audit row carries runId and status only, never output. */
export function testRunEndpoints(runner: TestRunner): Record<string, Endpoint> {
  return {
    async "connection.host_key_scan"(c) {
      const body = c.req.body && typeof c.req.body === "object" ? c.req.body as { host?: unknown; port?: unknown } : {};
      try { return { status: 200, body: await runner.scanHostKey(c.workspace!.id, body, c.signal) }; }
      catch (e) {
        if (e instanceof ApiError && e.code === "too_many_runs") return { status: 429, headers: { "retry-after": "5" }, body: { error: { code: e.code, message: e.message } } };
        throw e;
      }
    },
    async "test_run.execute"(c) {
      const b = c.req.body && typeof c.req.body === "object" && !Array.isArray(c.req.body) ? (c.req.body as TestRunRequest) : {};
      let r;
      try { r = await runner.run(c.workspace!.id, b, c.signal); }
      catch (e) {
        if (e instanceof ApiError && e.code === "too_many_runs") return { status: 429, headers: { "retry-after": "5" }, body: { error: { code: e.code, message: e.message } } };
        throw e;
      }
      return { status: r.status, body: r.body, audit: r.audit ? { resourceId: r.audit.after.runId, after: r.audit.after } : undefined };
    },
  };
}
