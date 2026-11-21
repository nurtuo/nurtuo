// SPDX-License-Identifier: AGPL-3.0-only
// MCP server v1 tools (M1): search, get_flow, apply_ops, validate, dry_run.
// deploy / replay_dlq require human approval (M2).
export const MCP_TOOLS_V1 = ["search", "get_flow", "apply_ops", "validate", "dry_run"] as const;
