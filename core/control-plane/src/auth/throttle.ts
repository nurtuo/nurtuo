// SPDX-License-Identifier: AGPL-3.0-only
// In-process throttles. Single-instance only: with several control-plane replicas put a shared
// limiter (or the reverse proxy) in front; the per-account lockout is persisted in the database.

/** Fixed-window counter: at most `max` hits per `windowMs` per key. */
export class RateLimiter {
  private hits = new Map<string, { n: number; reset: number }>();
  constructor(private max: number, private windowMs: number, private now: () => number = Date.now) {}
  /** Returns 0 if allowed, else the seconds until the window resets. */
  hit(key: string): number {
    const t = this.now();
    if (this.hits.size > 10_000) for (const [k, v] of this.hits) if (v.reset <= t) this.hits.delete(k);
    const cur = this.hits.get(key);
    if (!cur || cur.reset <= t) { this.hits.set(key, { n: 1, reset: t + this.windowMs }); return 0; }
    cur.n += 1;
    return cur.n > this.max ? Math.max(1, Math.ceil((cur.reset - t) / 1000)) : 0;
  }
}

/** Consecutive-failure lockout for identifiers that have no user row (so unknown emails behave like real ones). */
export class FailureTracker {
  private s = new Map<string, { n: number; until: number }>();
  constructor(private max: number, private lockMs: number, private now: () => number = Date.now) {}
  lockedFor(key: string): number {
    const v = this.s.get(key);
    return v && v.until > this.now() ? Math.ceil((v.until - this.now()) / 1000) : 0;
  }
  fail(key: string): void {
    if (this.s.size > 10_000) for (const [k, v] of this.s) if (v.until <= this.now()) this.s.delete(k);
    const v = this.s.get(key) ?? { n: 0, until: 0 };
    v.n += 1;
    if (v.n >= this.max) { v.until = this.now() + this.lockMs; v.n = 0; }
    this.s.set(key, v);
  }
  clear(key: string): void { this.s.delete(key); }
}
