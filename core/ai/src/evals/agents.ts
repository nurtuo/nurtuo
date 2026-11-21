// SPDX-License-Identifier: AGPL-3.0-only
// Agents drive an EvalSession. `ScriptedAgent` replays a fixed op list (deterministic baseline),
// `SuggestionAgent` follows suggest_next_steps, and `ToolLoopAgent` is the adapter an LLM plugs into.
import type { EvalSession, ToolSpec } from "./session.js";
import type { Op, Task } from "./scenario.js";

export interface Agent {
  readonly name: string;
  run(task: Task, session: EvalSession): Promise<void>;
}

/** Sends batches of ops in order, then validates. If a batch fails, the agent stops (a real failure to grade). */
export class ScriptedAgent implements Agent {
  readonly name: string;
  constructor(private readonly batches: Op[][], opts: { name?: string; create?: boolean } = {}) {
    this.name = opts.name ?? "scripted";
    this.create = opts.create ?? true;
  }
  private readonly create: boolean;

  async run({ scenario }: Task, s: EvalSession): Promise<void> {
    await s.call("list_connectors");
    await s.call("list_step_kinds");
    let first = this.create;
    for (const ops of this.batches) {
      const r = await s.call("apply_ops", {
        flowId: scenario.flowId, ops,
        ...(first ? { create: { name: scenario.flowName } } : {}),
      });
      first = false;
      if (r.isError) return;
    }
    await s.call("validate", { flowId: scenario.flowId });
  }
}

/** Starts with a `start` step and then follows the first candidate of suggest_next_steps until it reports done. */
export class SuggestionAgent implements Agent {
  readonly name = "suggestion-follower";
  constructor(private readonly startConfig: Record<string, unknown>, private readonly maxIterations = 12) {}

  async run({ scenario }: Task, s: EvalSession): Promise<void> {
    const begin = await s.call("apply_ops", {
      flowId: scenario.flowId, create: { name: scenario.flowName },
      ops: [{ op: "addStep", step: { id: "start", kind: "start", config: this.startConfig } }],
    });
    if (begin.isError) return;
    for (let i = 0; i < this.maxIterations; i++) {
      const sug = (await s.call("suggest_next_steps", { flowId: scenario.flowId })).body;
      if (sug.done) break;
      const op = sug.suggestions?.[0]?.candidates?.[0]?.op;
      if (!op) break;
      const r = await s.call("apply_ops", { flowId: scenario.flowId, ops: [op] });
      if (r.isError) break;
    }
    await s.call("validate", { flowId: scenario.flowId });
  }
}

// ------------------------------------------------------------------ LLM plug-in point

export type ChatMessage =
  | { role: "user"; text: string }
  | { role: "assistant"; text?: string; toolCalls?: { id: string; name: string; args: Record<string, unknown> }[] }
  | { role: "tool"; callId: string; name: string; text: string };

export interface ModelTurn {
  text?: string;
  toolCalls?: { id: string; name: string; args: Record<string, unknown> }[];
}

/**
 * Adapter an LLM provider implements: given the conversation and the MCP tool specs, produce the next
 * assistant turn. Map `tools` to your provider's tool format (`inputSchema` is JSON Schema) and map
 * the response back. Keep it stateless; the agent owns the transcript.
 */
export interface ChatModel {
  readonly name: string;
  next(messages: ChatMessage[], tools: ToolSpec[]): Promise<ModelTurn>;
}

/** Generic tool-use loop: feed the task prompt, execute each requested tool via MCP, return results, repeat. */
export class ToolLoopAgent implements Agent {
  readonly name: string;
  constructor(private readonly model: ChatModel, private readonly maxTurns = 25) {
    this.name = `llm:${model.name}`;
  }

  async run(task: Task, s: EvalSession): Promise<void> {
    const tools = await s.tools();
    const messages: ChatMessage[] = [{ role: "user", text: task.prompt }];
    for (let turn = 0; turn < this.maxTurns; turn++) {
      const out = await this.model.next(messages, tools);
      messages.push({ role: "assistant", ...(out.text ? { text: out.text } : {}), ...(out.toolCalls ? { toolCalls: out.toolCalls } : {}) });
      if (!out.toolCalls?.length) return; // model is done
      for (const c of out.toolCalls) {
        const r = await s.call(c.name, c.args);
        messages.push({ role: "tool", callId: c.id, name: c.name, text: JSON.stringify(r.body) });
      }
    }
  }
}
