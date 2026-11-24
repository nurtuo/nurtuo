// SPDX-License-Identifier: AGPL-3.0-only
import { ApiError } from "../domain.js";
import type { Endpoint } from "../auth/endpoints.js";
import type { TestRunner } from "./runner.js";

/** Fetch an SSH host key without saving it. */
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
  };
}
