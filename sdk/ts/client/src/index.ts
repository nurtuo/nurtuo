// SPDX-License-Identifier: Apache-2.0
// @nurtuo/client: small dependency-free fetch client for the Nurtuo control-plane REST API.
// Wire types are generated from core/control-plane/openapi.yaml (see scripts/gen-types.mjs).
import type { components } from "./schema.generated.js";

export type { paths, components, operations } from "./schema.generated.js";

type Schemas = components["schemas"];
export type Component = Schemas["Component"];
export type ComponentKind = Schemas["ComponentKind"];
export type ComponentCreate = Schemas["ComponentCreate"];
export type Revision = Schemas["Revision"];
export type Runtime = Schemas["Runtime"];
export type DesiredState = Schemas["DesiredState"];
export type Deployment = Schemas["Deployment"];
export type DeploymentStatus = Schemas["DeploymentStatus"];

export interface RuntimeRegistration {
  id: string;
  name: string;
  environment: string;
  version?: string;
  labels?: Record<string, string>;
}

/** Either `revisionId` or `revision` (the per-component number) must be given. */
export interface DeployRequest {
  componentId: string;
  runtimeId: string;
  revisionId?: string;
  revision?: number;
}
export interface DeployResult {
  deployment: Deployment;
  desiredState: DesiredState;
}

/** Non-2xx response from the API. `code` and `message` come from the `{error:{code,message}}` body. */
export class ApiError extends Error {
  override readonly name = "ApiError";
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly body?: unknown,
  ) {
    super(message);
  }
}

/** The request never produced an HTTP response (DNS, connection refused, timeout, abort). */
export class NetworkError extends Error {
  override readonly name = "NetworkError";
  constructor(message: string, readonly timedOut = false, options?: { cause?: unknown }) {
    super(message, options);
  }
}

export interface ClientOptions {
  /** Server root, e.g. `https://nurtuo.example.com` (a trailing slash is ignored). */
  baseUrl: string;
  /** Sent as `Authorization: Bearer <token>`. */
  token?: string;
  /** Per-attempt timeout in milliseconds. Default 30000; 0 disables. */
  timeoutMs?: number;
  /** Extra attempts after the first for retryable failures. Default 2. */
  retries?: number;
  /** Base backoff in milliseconds (doubled per attempt, plus jitter). Default 200. */
  retryDelayMs?: number;
  /** Also retry POST/PATCH (not idempotent). Default false. */
  retryUnsafe?: boolean;
  /** Extra headers on every request. */
  headers?: Record<string, string>;
  /** Override fetch (tests, proxies). */
  fetch?: typeof fetch;
}

const RETRY_STATUS = new Set([408, 429, 500, 502, 503, 504]);
const IDEMPOTENT = new Set(["GET", "HEAD", "PUT", "DELETE", "OPTIONS"]);

interface RequestOptions {
  query?: Record<string, string | undefined>;
  body?: unknown;
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

function retryAfterMs(res: Response): number | undefined {
  const h = res.headers.get("retry-after");
  if (!h) return undefined;
  const secs = Number(h);
  if (Number.isFinite(secs)) return Math.max(0, secs * 1000);
  const when = Date.parse(h);
  return Number.isNaN(when) ? undefined : Math.max(0, when - Date.now());
}

export class NurtuoClient {
  private readonly baseUrl: string;
  private readonly opts: Required<Pick<ClientOptions, "timeoutMs" | "retries" | "retryDelayMs" | "retryUnsafe">> & ClientOptions;

  constructor(options: ClientOptions) {
    if (!options.baseUrl) throw new TypeError("baseUrl is required");
    this.baseUrl = options.baseUrl.replace(/\/+$/, "");
    this.opts = { timeoutMs: 30_000, retries: 2, retryDelayMs: 200, retryUnsafe: false, ...options };
  }

  // ---- components ----
  async listComponents(): Promise<Component[]> {
    return (await this.request<{ items?: Component[] }>("GET", "/v1/components")).items ?? [];
  }
  getComponent(id: string): Promise<Component> {
    return this.request("GET", `/v1/components/${enc(id)}`);
  }
  createComponent(input: ComponentCreate): Promise<Component> {
    return this.request("POST", "/v1/components", { body: input });
  }
  updateComponent(id: string, patch: { description?: string }): Promise<Component> {
    return this.request("PATCH", `/v1/components/${enc(id)}`, { body: patch });
  }
  async deleteComponent(id: string): Promise<void> {
    await this.request("DELETE", `/v1/components/${enc(id)}`);
  }

  // ---- revisions ----
  async listRevisions(componentId: string): Promise<Revision[]> {
    return (await this.request<{ items?: Revision[] }>("GET", `/v1/components/${enc(componentId)}/revisions`)).items ?? [];
  }
  createRevision(componentId: string, content: object): Promise<Revision> {
    return this.request("POST", `/v1/components/${enc(componentId)}/revisions`, { body: { content } });
  }
  getRevision(componentId: string, number: number): Promise<Revision> {
    return this.request("GET", `/v1/components/${enc(componentId)}/revisions/${number}`);
  }

  // ---- runtimes ----
  async listRuntimes(filter: { environment?: string } = {}): Promise<Runtime[]> {
    return (await this.request<{ items?: Runtime[] }>("GET", "/v1/runtimes", { query: filter })).items ?? [];
  }
  getRuntime(id: string): Promise<Runtime> {
    return this.request("GET", `/v1/runtimes/${enc(id)}`);
  }
  registerRuntime(input: RuntimeRegistration): Promise<Runtime> {
    return this.request("POST", "/v1/runtimes", { body: input });
  }
  getDesiredState(runtimeId: string): Promise<DesiredState> {
    return this.request("GET", `/v1/runtimes/${enc(runtimeId)}/desired-state`);
  }

  // ---- deployments ----
  async listDeployments(filter: { runtimeId?: string; componentId?: string } = {}): Promise<Deployment[]> {
    return (await this.request<{ items?: Deployment[] }>("GET", "/v1/deployments", { query: filter })).items ?? [];
  }
  deploy(input: DeployRequest): Promise<DeployResult> {
    return this.request("POST", "/v1/deployments", { body: input });
  }
  getDeployment(id: string): Promise<Deployment> {
    return this.request("GET", `/v1/deployments/${enc(id)}`);
  }
  /** Runtime-side status report (normally sent by the gateway). */
  reportDeploymentStatus(id: string, status: DeploymentStatus, message?: string): Promise<Deployment> {
    return this.request("PUT", `/v1/deployments/${enc(id)}/status`, { body: message === undefined ? { status } : { status, message } });
  }

  // ---- transport ----
  async request<T = unknown>(method: string, path: string, o: RequestOptions = {}): Promise<T> {
    const url = new URL(this.baseUrl + path);
    for (const [k, v] of Object.entries(o.query ?? {})) if (v !== undefined) url.searchParams.set(k, v);
    const headers: Record<string, string> = { accept: "application/json", ...this.opts.headers };
    if (this.opts.token) headers["authorization"] = `Bearer ${this.opts.token}`;
    let body: string | undefined;
    if (o.body !== undefined) {
      body = JSON.stringify(o.body);
      headers["content-type"] = "application/json";
    }
    const canRetry = IDEMPOTENT.has(method) || this.opts.retryUnsafe;
    const doFetch = this.opts.fetch ?? fetch;

    for (let attempt = 0; ; attempt++) {
      const more = canRetry && attempt < this.opts.retries;
      const backoff = this.opts.retryDelayMs * 2 ** attempt * (0.5 + Math.random() / 2);
      let res: Response;
      const ac = new AbortController();
      const timer = this.opts.timeoutMs > 0 ? setTimeout(() => ac.abort(), this.opts.timeoutMs) : undefined;
      try {
        res = await doFetch(url, { method, headers, body, signal: ac.signal });
      } catch (e) {
        const timedOut = ac.signal.aborted;
        if (more) {
          await sleep(backoff);
          continue;
        }
        throw new NetworkError(
          timedOut ? `${method} ${path}: timed out after ${this.opts.timeoutMs}ms` : `${method} ${path}: ${(e as Error).message}${causeOf(e)}`,
          timedOut,
          { cause: e },
        );
      } finally {
        if (timer) clearTimeout(timer);
      }

      if (res.ok) {
        if (res.status === 204) return undefined as T;
        const text = await res.text();
        return (text ? JSON.parse(text) : undefined) as T;
      }
      if (more && RETRY_STATUS.has(res.status)) {
        await res.body?.cancel().catch(() => undefined);
        await sleep(Math.min(retryAfterMs(res) ?? backoff, 10_000));
        continue;
      }
      throw await toApiError(res);
    }
  }
}

function causeOf(e: unknown): string {
  const c = (e as { cause?: { code?: string; message?: string } } | undefined)?.cause;
  return c?.code || c?.message ? ` (${c.code ?? c.message})` : "";
}

async function toApiError(res: Response): Promise<ApiError> {
  const text = await res.text().catch(() => "");
  let body: unknown;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    body = text;
  }
  const err = (body as { error?: { code?: string; message?: string } } | undefined)?.error;
  return new ApiError(res.status, err?.code ?? `http_${res.status}`, err?.message ?? (res.statusText || `HTTP ${res.status}`), body);
}

const enc = encodeURIComponent;
