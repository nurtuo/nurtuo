// SPDX-License-Identifier: AGPL-3.0-only
import { ApiError } from "../domain.js";
import type { ConnectionRepository } from "./repository.js";
import type { SecretStore } from "./secrets.js";

export class ConnectionCoordinator {
  /** Present key = lock held; value = FIFO of waiting grants. */
  private queues = new Map<string, Array<() => void>>();
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

  /**
   * Serialize work per workspace. A queued acquisition is removed when `signal` aborts, so cancelled waiters never
   * stay chained behind a stalled holder. A holder whose signal aborts gives up the lock as soon as it is outside
   * a `critical` section; critical sections (reconciliation writes) always finish under the lock.
   */
  async withWorkspace<T>(workspace: string, work: (lease: WorkspaceLease) => Promise<T>, signal?: AbortSignal): Promise<T> {
    const release = await this.acquire(workspace, signal);
    let critical = 0;
    let abandoned = false;
    const onAbort = () => { abandoned = true; if (!critical) release(); };
    signal?.addEventListener("abort", onAbort, { once: true });
    if (signal?.aborted) onAbort();
    const lease: WorkspaceLease = {
      critical: async (fn) => {
        critical++;
        try { return await fn(); }
        finally { critical--; if (abandoned && !critical) release(); }
      },
    };
    try {
      return await work(lease);
    } finally {
      signal?.removeEventListener("abort", onAbort);
      release();
    }
  }

  /** Number of queued (not yet granted) waiters for a workspace; for tests. */
  waiting(workspace: string): number { return this.queues.get(workspace)?.length ?? 0; }

  private acquire(workspace: string, signal?: AbortSignal): Promise<() => void> {
    signal?.throwIfAborted();
    let queue = this.queues.get(workspace);
    const make = () => {
      let done = false;
      return () => {
        if (done) return;
        done = true;
        const q = this.queues.get(workspace)!;
        const next = q.shift();
        if (next) next(); else this.queues.delete(workspace);
      };
    };
    if (!queue) {
      this.queues.set(workspace, []);
      return Promise.resolve(make());
    }
    queue = this.queues.get(workspace)!;
    return new Promise((resolve, reject) => {
      const grant = () => { signal?.removeEventListener("abort", onAbort); resolve(make()); };
      const onAbort = () => {
        const i = queue!.indexOf(grant);
        if (i >= 0) queue!.splice(i, 1);
        reject(signal!.reason);
      };
      queue!.push(grant);
      signal?.addEventListener("abort", onAbort, { once: true });
    });
  }
}

export interface WorkspaceLease {
  /** Run `fn` so that an aborted holder keeps the lock until it finishes. */
  critical<R>(fn: () => Promise<R>): Promise<R>;
}
