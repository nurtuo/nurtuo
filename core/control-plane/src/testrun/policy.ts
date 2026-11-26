// SPDX-License-Identifier: AGPL-3.0-only
// What a server-side test run may do (canvas-run spec D11): sftp only, read-only operations, an
// explicit connection on every connector step. Checked before anything is spawned.
import { ApiError } from "../domain.js";

export const ALLOWED_OPERATIONS = ["GET", "QUERY"] as const;

export interface FlowStep { id: string; kind: string; config?: Record<string, unknown> }
export interface ConnectorUse { stepId: string; connection: string }

/** Throws 400 not_allowed_in_test_run / connection_required; returns the connection each connector step uses. */
export function checkFlowPolicy(steps: readonly FlowStep[]): ConnectorUse[] {
  const uses: ConnectorUse[] = [];
  for (const s of steps) {
    if (s.kind !== "connector") continue;
    const cfg = s.config ?? {};
    if (cfg.connector !== "sftp") throw new ApiError(400, "not_allowed_in_test_run", `step ${s.id}: connector "${String(cfg.connector)}" is not allowed in test runs (only sftp)`);
    // The engine upper-cases the operation, so "get" works and must be allowed; anything else is refused.
    const op = typeof cfg.operation === "string" ? cfg.operation.toUpperCase() : "";
    if (!(ALLOWED_OPERATIONS as readonly string[]).includes(op)) throw new ApiError(400, "not_allowed_in_test_run", `step ${s.id}: operation "${String(cfg.operation)}" is not allowed in test runs (only ${ALLOWED_OPERATIONS.join(" and ")})`);
    if (typeof cfg.connection !== "string" || cfg.connection === "") throw new ApiError(400, "connection_required", `step ${s.id}: choose a connection for this connector step`);
    uses.push({ stepId: s.id, connection: cfg.connection });
  }
  return uses;
}
