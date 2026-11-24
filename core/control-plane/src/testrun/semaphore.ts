// SPDX-License-Identifier: AGPL-3.0-only
// Concurrency limits for runs (canvas-run spec D2): per workspace and per instance. A held slot is
// released exactly once, in `finally`, so an error never leaks a slot.
export class RunLimiter {
  private perWs = new Map<string, number>();
  private total = 0;
  constructor(readonly perWorkspace = 2, readonly perInstance = 4) {}

  /** Returns a release function, or undefined when a limit is reached. */
  tryAcquire(workspaceId: string): (() => void) | undefined {
    const n = this.perWs.get(workspaceId) ?? 0;
    if (n >= this.perWorkspace || this.total >= this.perInstance) return undefined;
    this.perWs.set(workspaceId, n + 1);
    this.total++;
    let done = false;
    return () => {
      if (done) return;
      done = true;
      this.total--;
      const m = (this.perWs.get(workspaceId) ?? 1) - 1;
      if (m <= 0) this.perWs.delete(workspaceId); else this.perWs.set(workspaceId, m);
    };
  }
}
