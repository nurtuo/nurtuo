// SPDX-License-Identifier: AGPL-3.0-only
// Interface a runtime implements to support the `dry_run` tool. The MCP server never executes
// flows itself; tests use a fake.
import type { Flow } from "@nurtuo/flow-schema";

export interface DryRunStepTrace {
  stepId: string;
  /** Port the step emitted on, if any. */
  port?: string;
  output?: unknown;
}

export interface DryRunResult {
  ok: boolean;
  trace: DryRunStepTrace[];
  output?: unknown;
  error?: string;
}

export interface DryRunner {
  dryRun(flow: Flow, input: unknown): Promise<DryRunResult>;
}
