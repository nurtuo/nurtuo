// SPDX-License-Identifier: AGPL-3.0-only
import { ApiError } from "../domain.js";
import type { ConnectionRepository } from "./repository.js";
import type { SecretStore } from "./secrets.js";

export class ConnectionCoordinator {
  private tails = new Map<string, Promise<void>>();
  readonly pending = new Map<string, Map<string, { created: string[]; retired: string[]; generation: string }>>();

  /** Reconcile under the workspace lock so a staged value remains available until its fate is known. */
  async reconcile(connections: ConnectionRepository, secrets: SecretStore, workspace: string, id: string) {
    const saves = this.pending.get(workspace);
    const pending = saves?.get(id);
    if (!saves || !pending) return undefined;
    let current;
    try { current = await connections.get(workspace, id); }
    catch { throw new ApiError(503, "save_outcome_unknown", "connection save outcome could not be determined"); }
    const committed = current?.generation === pending.generation;
    for (const name of committed ? pending.retired : pending.created) await secrets.delete(workspace, name);
    saves.delete(id);
    if (!saves.size) this.pending.delete(workspace);
    return committed ? current : undefined;
  }

  async withWorkspace<T>(workspace: string, work: () => Promise<T>): Promise<T> {
    const previous = this.tails.get(workspace);
    let release!: () => void;
    const tail = new Promise<void>((resolve) => { release = resolve; });
    this.tails.set(workspace, tail);
    try {
      await previous;
      return await work();
    } finally {
      release();
      if (this.tails.get(workspace) === tail) this.tails.delete(workspace);
    }
  }
}
