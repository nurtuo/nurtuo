// SPDX-License-Identifier: AGPL-3.0-only
// Minimal control-plane client for the canvas (same-origin session cookie + CSRF header).
// The CSRF token is kept in memory only; after a reload it is restored from GET /v1/auth/me.

export class ApiError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message);
  }
}

/** The control plane could not be reached (offline editing keeps working). */
export class OfflineError extends Error {}

export interface WorkspaceInfo {
  id: string;
  slug: string;
  name: string;
  role: string;
  permissions: string[];
}
export interface MeResponse {
  user: { id?: string; email?: string } | null;
  csrfToken?: string;
  workspaces: WorkspaceInfo[];
}
export interface ConnectionItem {
  id: string;
  connector: string;
  settings: Record<string, unknown>;
  secrets: Record<string, "set" | "missing">;
  updatedAt?: string;
}
type FetchFn = (input: string, init?: RequestInit) => Promise<Response>;

const MUTATING = new Set(["POST", "PUT", "PATCH", "DELETE"]);

export class ControlPlane {
  private csrf: string | null = null;

  constructor(private readonly fetchFn: FetchFn = (i, init) => fetch(i, init)) {}

  get csrfToken(): string | null {
    return this.csrf;
  }

  private async request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const headers: Record<string, string> = {};
    if (body !== undefined) headers["content-type"] = "application/json";
    if (MUTATING.has(method) && this.csrf) headers["x-nurtuo-csrf"] = this.csrf;
    let res: Response;
    try {
      res = await this.fetchFn(path, {
        method,
        headers,
        credentials: "same-origin",
        ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
        ...(signal ? { signal } : {}),
      });
    } catch (e) {
      if (e instanceof DOMException && e.name === "AbortError") throw e;
      throw new OfflineError(e instanceof Error ? e.message : String(e));
    }
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    let json: unknown;
    try {
      json = text ? JSON.parse(text) : undefined;
    } catch {
      // A dev proxy without a backend answers with a non-JSON error page.
      if (res.status >= 500) throw new OfflineError(`control plane unreachable (${res.status})`);
      throw new ApiError(res.status, "bad_response", `unexpected response (${res.status})`);
    }
    if (!res.ok) {
      const e = (json as { error?: { code?: string; message?: string } } | undefined)?.error;
      throw new ApiError(res.status, e?.code ?? "error", e?.message ?? `request failed (${res.status})`);
    }
    return json as T;
  }

  async login(email: string, password: string): Promise<MeResponse | null> {
    const r = await this.request<{ csrfToken: string }>("POST", "/v1/auth/login", { email, password });
    this.csrf = r.csrfToken;
    return this.me();
  }

  /** Returns null when there is no valid session. Restores the CSRF token. */
  async me(): Promise<MeResponse | null> {
    try {
      const r = await this.request<MeResponse>("GET", "/v1/auth/me");
      if (r.csrfToken) this.csrf = r.csrfToken;
      return { ...r, workspaces: r.workspaces ?? [] };
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) return null;
      throw e;
    }
  }

  async logout(): Promise<void> {
    try {
      await this.request("POST", "/v1/auth/logout");
    } catch (error) {
      if (!(error instanceof ApiError && error.status === 401)) throw error;
    }
    this.csrf = null;
  }

  private ws = (ws: string) => `/v1/workspaces/${encodeURIComponent(ws)}`;

  async listConnections(ws: string, signal?: AbortSignal): Promise<ConnectionItem[]> {
    return (await this.request<{ items: ConnectionItem[] }>("GET", `${this.ws(ws)}/connections`, undefined, signal)).items;
  }
  putConnection(ws: string, id: string, body: { connector: string; settings: Record<string, unknown>; credentials?: Record<string, string | null> }): Promise<unknown> {
    return this.request("PUT", `${this.ws(ws)}/connections/${encodeURIComponent(id)}`, body);
  }
  scanHostKey(ws: string, host: string, port: number): Promise<{ keys: { type: string; fingerprintSha256: string }[] }> {
    return this.request("POST", `${this.ws(ws)}/connections/host-key-scan`, { host, port });
  }
  deleteConnection(ws: string, id: string): Promise<void> {
    return this.request("DELETE", `${this.ws(ws)}/connections/${encodeURIComponent(id)}`);
  }

}

/** Canvas-facing text for API failures of a test run (spec "Failure modes"). */
export function describeApiError(e: unknown): string {
  if (e instanceof OfflineError) return "Control plane unreachable; the canvas keeps working offline.";
  if (!(e instanceof ApiError)) return e instanceof Error ? e.message : String(e);
  switch (true) {
    case e.status === 401:
      return "Session expired: sign in again.";
    case e.status === 403:
      return "Your role cannot run flows.";
    case e.status === 429:
      return "Another run is in progress, retry.";
    case e.status === 503:
      return "Test runs are not configured on this server.";
    case e.code === "secret_missing":
      return `${e.message} Open Connections and enter the password again.`;
    default:
      return e.message;
  }
}
