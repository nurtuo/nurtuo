// SPDX-License-Identifier: AGPL-3.0-only
// Scenario = a goal plus machine-checkable expectations about the flow an agent leaves behind.
// The same scenarios grade a scripted agent today and an LLM agent later.
import type { Flow } from "@nurtuo/flow-schema";
import type { FlowStore } from "@nurtuo/mcp";

export type Op = Record<string, unknown>;

export interface Expectations {
  /** The stored flow must have no validation errors (always checked; set false to expect failure). */
  valid?: boolean;
  /** At least this many steps of each kind. */
  minStepKinds?: Partial<Record<string, number>>;
  /** Each pair must appear as a `connector` step (operation compared case-insensitively). */
  connectors?: { connector: string; operation: string }[];
  /** Exact `start` config properties to match, e.g. { type: "listener", connector: "http" }. */
  start?: Record<string, unknown>;
  /** Edge keys that must exist, e.g. "found:false". */
  edges?: string[];
  /** The shortest path from start must pass through these step kinds in order. */
  pathKinds?: string[];
  /** Upper bound on tool calls the agent may use. */
  maxToolCalls?: number;
  /** Flow must be at least this revision (the agent really wrote it). */
  minRevision?: number;
}

export interface Scenario {
  id: string;
  title: string;
  /** What a human (or LLM) is asked to build. This text is the LLM's task. */
  goal: string;
  flowId: string;
  /** Flow name to create. */
  flowName: string;
  /** Prepares the store before the agent runs (e.g. seeds a broken flow to repair). */
  setup?: (store: FlowStore) => Promise<void>;
  expect: Expectations;
}

export interface Task {
  scenario: Scenario;
  /** Rendered guided prompt (the `build-integration` MCP prompt) an LLM agent can use as its instructions. */
  prompt: string;
}

export interface Check {
  name: string;
  passed: boolean;
  detail?: string;
}

export interface ScenarioResult {
  scenario: string;
  agent: string;
  passed: boolean;
  checks: Check[];
  toolCalls: number;
  flow: Flow | undefined;
  durationMs: number;
}
