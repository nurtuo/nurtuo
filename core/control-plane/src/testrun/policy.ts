// SPDX-License-Identifier: AGPL-3.0-only
// Canvas Run D11 and #94 allow selected read-only operations on explicit workspace connections.
import { ApiError } from "../domain.js";

const ALLOWED_OPERATIONS = new Map<string, readonly string[]>([
  ["sftp", ["GET", "QUERY"]],
  ["http", ["GET"]],
  ["postgres", ["QUERY"]],
]);

export interface FlowStep { id: string; kind: string; config?: Record<string, unknown> }
export interface ConnectorUse { stepId: string; connection: string; connector: string }

/** Throws 400 not_allowed_in_test_run / connection_required; returns the connection each connector step uses. */
export function checkFlowPolicy(steps: readonly FlowStep[]): ConnectorUse[] {
  const uses: ConnectorUse[] = [];
  for (const s of steps) {
    if (s.kind !== "connector") continue;
    const cfg = s.config ?? {};
    const connector = typeof cfg.connector === "string" ? cfg.connector : "";
    const operations = ALLOWED_OPERATIONS.get(connector);
    if (!operations) throw new ApiError(400, "not_allowed_in_test_run", `step ${s.id}: connector is not allowed in test runs`);
    const op = typeof cfg.operation === "string" ? cfg.operation.toUpperCase() : "";
    if (!operations.includes(op)) throw new ApiError(400, "not_allowed_in_test_run", `step ${s.id}: choose ${operations.join(" or ")} for ${connector}`);
    if (typeof cfg.connection !== "string" || cfg.connection === "") throw new ApiError(400, "connection_required", `step ${s.id}: choose a connection for this connector step`);
    uses.push({ stepId: s.id, connection: cfg.connection, connector });
  }
  return uses;
}
