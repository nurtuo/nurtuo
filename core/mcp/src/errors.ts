// SPDX-License-Identifier: AGPL-3.0-only
// Structured error codes returned by every tool. Agents can branch on `code` instead of parsing text.

export const ERROR_CODES = [
  "not_found",          // flow / runtime / deployment does not exist
  "invalid_input",      // arguments are well-typed but semantically wrong
  "limit_exceeded",     // input or output over a guardrail limit
  "secret_in_input",    // literal credentials in ops; use connection references instead
  "revision_conflict",  // optimistic concurrency check failed; re-read and retry
  "timeout",            // per-call deadline exceeded
  "deploy_disabled",    // deploy attempted without --allow-deploy
  "invalid_flow",       // flow has validation errors and the action requires a valid flow
  "unavailable",        // optional capability (runtime, deployer) not configured
  "unauthorized",       // backend rejected our credentials
  "backend_error",      // backend (control plane) failed or is unreachable
  "internal",           // bug
] as const;
export type ErrorCode = (typeof ERROR_CODES)[number];

export class ToolError extends Error {
  constructor(readonly code: ErrorCode, message: string, readonly details?: Record<string, unknown>) {
    super(message);
    this.name = "ToolError";
  }
}
