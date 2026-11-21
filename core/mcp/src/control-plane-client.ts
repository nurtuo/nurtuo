// SPDX-License-Identifier: AGPL-3.0-only
// Minimal fetch client for the control-plane REST API (core/control-plane/openapi.yaml). Kept
// internal and dependency-free on purpose: @nurtuo/control-plane depends on this package, not the
// other way round.

export type FetchLike = (url: string, init: { method: string; headers: Record<string, string>; body?: string; signal?: AbortSignal }) => Promise<{
  status: number;
  text(): Promise<string>;
}>;

export class ControlPlaneError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message);
    this.name = "ControlPlaneError";
  }
}

export interface ControlPlaneClientOptions {
  /** e.g. http://localhost:8080 (no trailing /v1). */
  baseUrl: string;
  /** Bearer token; sent as `Authorization: Bearer ...`. Never logged or included in errors. */
  token?: string;
  fetch?: FetchLike;
  /** Per-request timeout, ms (default 15000). */
  timeoutMs?: number;
}

export class ControlPlaneClient {
  readonly #base: string;
  readonly #token: string | undefined;
  readonly #fetch: FetchLike;
  readonly #timeoutMs: number;

  constructor(opts: ControlPlaneClientOptions) {
    this.#base = opts.baseUrl.replace(/\/+$/, "");
    this.#token = opts.token;
    this.#fetch = opts.fetch ?? ((url, init) => fetch(url, init));
    this.#timeoutMs = opts.timeoutMs ?? 15_000;
  }

  async request<T = unknown>(method: string, path: string, opts: { body?: unknown; query?: Record<string, string | undefined> } = {}): Promise<T> {
    const qs = Object.entries(opts.query ?? {}).filter((e): e is [string, string] => e[1] !== undefined);
    const url = `${this.#base}${path}${qs.length ? `?${new URLSearchParams(qs)}` : ""}`;
    const headers: Record<string, string> = { accept: "application/json" };
    if (opts.body !== undefined) headers["content-type"] = "application/json";
    if (this.#token) headers["authorization"] = `Bearer ${this.#token}`;
    let res: { status: number; text(): Promise<string> };
    try {
      res = await this.#fetch(url, {
        method, headers,
        ...(opts.body !== undefined ? { body: JSON.stringify(opts.body) } : {}),
        signal: AbortSignal.timeout(this.#timeoutMs),
      });
    } catch (e) {
      const name = (e as { name?: string }).name;
      if (name === "TimeoutError" || name === "AbortError") {
        throw new ControlPlaneError(0, "timeout", `control plane did not answer ${method} ${path} within ${this.#timeoutMs} ms`);
      }
      throw new ControlPlaneError(0, "unreachable", `control plane unreachable at ${this.#base}: ${(e as Error).message}`);
    }
    const raw = await res.text();
    let parsed: unknown;
    if (raw) {
      try { parsed = JSON.parse(raw); } catch { parsed = undefined; }
    }
    if (res.status >= 200 && res.status < 300) return parsed as T;
    const err = (parsed as { error?: { code?: string; message?: string } } | undefined)?.error;
    throw new ControlPlaneError(res.status, err?.code ?? `http_${res.status}`, err?.message ?? `${method} ${path} failed with HTTP ${res.status}`);
  }
}
