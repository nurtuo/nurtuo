// SPDX-License-Identifier: AGPL-3.0-only
// The single integration point with api.ts: createAuthHook(...).handle(req, next) wraps the router.
// Order: match policy (fail closed) -> authenticate -> CSRF -> instance permission -> resolve workspace -> authorize ->
// run handler -> audit (every mutating request by an authenticated caller, including denials).
import { randomUUID } from "node:crypto";
import { ApiError } from "../domain.js";
import type { ApiRequest, ApiResponse } from "../api.js";
import { sha256hex, safeEqual } from "./crypto.js";
import { endpoints, type AppendAudit, type AuditDetail, type Endpoint, type EndpointResult, type OidcSetup } from "./endpoints.js";
import { MUTATING, authorize, matchPolicy, type RoutePolicy } from "./policy.js";
import type { AuthService } from "./service.js";
import type { Principal, Workspace } from "./types.js";

export interface AuthHook { handle(req: ApiRequest, next: (r: ApiRequest) => Promise<ApiResponse>): Promise<ApiResponse> }
export interface AuthHookOptions {
  service: AuthService;
  oidc?: OidcSetup;
  /** Honour X-Forwarded-For (only behind a trusted reverse proxy). */
  trustProxy?: boolean;
  /** Called when the audit append fails; the request then fails with 500 audit_unavailable. */
  onAuditError?: (e: unknown) => void;
  /** Endpoints of other modules, keyed by policy action; built-in auth endpoints win on a name clash. They receive the resolved workspace. */
  extraEndpoints?: Record<string, Endpoint>;
  /** Called after a workspace was deleted so other modules can purge what they keyed by workspace id. */
  serializeWorkspaceDelete?: (workspaceId: string, remove: () => Promise<EndpointResult>) => Promise<EndpointResult>;
  onWorkspaceDeleted?: (workspaceId: string) => Promise<void>;
}

export const hashJson = (v: unknown): string | null => (v === undefined || v === null ? null : `sha256:${sha256hex(stableJson(v))}`);
function stableJson(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(stableJson).join(",")}]`;
  if (v && typeof v === "object") { const o = v as Record<string, unknown>; return `{${Object.keys(o).sort().map((k) => `${JSON.stringify(k)}:${stableJson(o[k])}`).join(",")}}`; }
  return JSON.stringify(v) ?? "null";
}

const errRes = (status: number, code: string, message: string, headers?: ApiResponse["headers"]): ApiResponse => ({ status, headers, body: { error: { code, message } } });

export function createAuthHook(o: AuthHookOptions): AuthHook {
  const { service } = o;
  const handlers: Record<string, Endpoint> = { ...o.extraEndpoints, ...endpoints };
  if (o.oidc?.jit && (o.oidc.jit.role === "owner" || o.oidc.jit.role === "admin")) throw new Error("OIDC JIT provisioning must not grant owner or admin");

  function clientIp(req: ApiRequest): string | null {
    if (o.trustProxy) {
      const xf = req.headers?.["x-forwarded-for"]?.split(",")[0]?.trim();
      if (xf) return xf.slice(0, 64);
    }
    return req.ip ?? null;
  }

  async function resolveWorkspace(policy: RoutePolicy, params: Record<string, string>, p: Principal, headers: Record<string, string | undefined>): Promise<Workspace | ApiResponse | undefined> {
    const repo = service.repo;
    if (policy.workspace === "none") return undefined;
    if (policy.workspace === "path") {
      const w = await repo.getWorkspace(params.ws!);
      // Existence of workspaces you do not belong to is not disclosed: 404, not 403.
      if (!w || !p.memberships.has(w.id) || (p.kind === "token" && p.tokenWorkspaceId !== w.id)) return errRes(404, "not_found", "workspace not found");
      return w;
    }
    const hdr = headers["x-nurtuo-workspace"];
    if (p.kind === "token") {
      if (hdr) { const hw = await repo.getWorkspace(hdr); if (!hw || hw.id !== p.tokenWorkspaceId) return errRes(403, "forbidden", "token is not valid for this workspace"); }
      return (await repo.getWorkspace(p.tokenWorkspaceId!)) ?? errRes(403, "forbidden", "workspace no longer exists");
    }
    if (hdr) {
      const hw = await repo.getWorkspace(hdr);
      if (!hw || !p.memberships.has(hw.id)) return errRes(403, "forbidden", "not a member of this workspace");
      return hw;
    }
    if (p.memberships.size === 1) return (await repo.getWorkspace([...p.memberships.keys()][0]!))!;
    if (p.memberships.size === 0) return errRes(403, "forbidden", "not a member of any workspace");
    return errRes(400, "workspace_required", "send X-Nurtuo-Workspace: <workspace id or slug>");
  }

  async function audit(req: ApiRequest, base: { requestId: string; ip: string | null; policy: RoutePolicy; params: Record<string, string>; principal: Principal | null; ws?: Workspace | null; status: number; detail?: AuditDetail; before?: unknown; after?: unknown; extra?: Record<string, unknown> }, append: AppendAudit = (entry) => service.repo.appendAudit(entry)) {
    const d = base.detail ?? {};
    const actor = d.actor ?? (base.principal ? { type: base.principal.kind, id: base.principal.tokenId ?? base.principal.userId } : { type: "anonymous" as const, id: null });
    await append({
      workspaceId: d.workspaceId !== undefined ? d.workspaceId : (base.ws?.id ?? null),
      actorType: actor.type, actorId: actor.id,
      action: base.policy.action, resource: base.policy.resource,
      resourceId: d.resourceId ?? base.params.id ?? base.params.userId ?? base.params.tokenId ?? null,
      beforeHash: hashJson(d.before !== undefined ? d.before : base.before),
      afterHash: hashJson(d.after !== undefined ? d.after : base.after),
      ip: base.ip, requestId: base.requestId, status: base.status,
      metadata: { method: req.method.toUpperCase(), ...(base.principal?.kind === "token" ? { userId: base.principal.userId } : {}), ...base.extra, ...d.metadata },
    });
  }

  return {
    async handle(req, next) {
      const hdrId = req.headers?.["x-request-id"];
      const requestId = hdrId && /^[A-Za-z0-9._-]{8,64}$/.test(hdrId) ? hdrId : randomUUID();
      const ip = clientIp(req);
      const finish = (r: ApiResponse): ApiResponse => ({ ...r, headers: { "x-request-id": requestId, "cache-control": "no-store", ...r.headers } });
      try {
        return finish(await inner(req, next, requestId, ip));
      } catch (e) {
        if (e instanceof ApiError) {
          const h = e.status === 401 ? { "www-authenticate": 'Bearer realm="nurtuo"' } : undefined;
          return finish(errRes(e.status, e.code, e.message, h));
        }
        throw e;
      }
    },
  };

  async function inner(req: ApiRequest, next: (r: ApiRequest) => Promise<ApiResponse>, requestId: string, ip: string | null): Promise<ApiResponse> {
    const method = req.method.toUpperCase();
    const headers = req.headers ?? {};
    const hit = matchPolicy(method, req.path);
    // Public routes ignore any (possibly stale) credentials; everything else throws 401 for presented-but-invalid ones.
    const principal = hit?.policy.access === "public" ? null : await service.authenticate(headers);

    if (!hit) {
      // Fail closed: a route nobody wrote a policy for is never reachable.
      if (!principal) throw new ApiError(401, "unauthenticated", "authentication required");
      return errRes(403, "no_policy", "no authorization policy for this route");
    }
    const { policy, params } = hit;
    const mutating = MUTATING.has(method);
    const query = req.query ?? {};
    const ctxBase = { req, principal, params, query, ip, requestId, service, oidc: o.oidc, signal: req.signal };
    const auditBase = { requestId, ip, policy, params, principal };

    let auditWritten = false;
    const run = async (ws?: Workspace): Promise<EndpointResult> => {
      const ep = handlers[policy.action];
      if (ep) return ep({
        ...ctxBase,
        workspace: ws,
        async auditMutation(status, detail, append) {
          await safeAudit(() => audit(req, { ...auditBase, ws, status, detail }, append));
          auditWritten = true;
        },
      });
      // Legacy route implemented by api.ts. Capture the before-state for PATCH/PUT/DELETE.
      let before: unknown;
      if (policy.beforePath && mutating) {
        const b = await next({ method: "GET", path: policy.beforePath(params), headers: {}, ip: ip ?? undefined });
        if (b.status === 200) before = structuredClone(b.body); // snapshot: in-memory repos return live objects
      }
      const res = await next(req);
      const created = res.body && typeof res.body === "object" ? (res.body as any).id : undefined;
      return { ...res, audit: { before, after: res.status < 300 ? res.body : undefined, resourceId: params.id ?? (typeof created === "string" ? created : null) } };
    };
    const strip = ({ audit: _a, ...r }: EndpointResult): ApiResponse => r;

    if (policy.access === "public") {
      const res = await run();
      // Anonymous traffic is only audited for credential endpoints (failed/successful logins), never generic noise.
      if (policy.action === "auth.login" || policy.action === "auth.oidc_callback") await safeAudit(() => audit(req, { ...auditBase, status: res.status, detail: res.audit }));
      return strip(res);
    }

    if (!principal) throw new ApiError(401, "unauthenticated", "authentication required");
    if (policy.userOnly && principal.kind !== "user") return errRes(403, "forbidden", "this endpoint requires a user session");
    if (principal.viaCookie && mutating) {
      const sent = headers["x-nurtuo-csrf"];
      if (!sent || !principal.csrf || !safeEqual(sent, principal.csrf)) return errRes(403, "csrf", "missing or invalid X-Nurtuo-Csrf header");
    }

    if (policy.access === "instance") {
      // Checked before the handler, so non-holders cannot probe body validation or slug conflicts.
      if (!(await service.instancePermissions(principal)).has(policy.instancePermission!)) {
        if (mutating) await safeAudit(() => audit(req, { ...auditBase, status: 403, extra: { denied: "forbidden", permission: policy.instancePermission } }));
        return errRes(403, "forbidden", `missing permission ${policy.instancePermission}`);
      }
    }

    let ws: Workspace | undefined;
    if (policy.workspace !== "none") {
      const r = await resolveWorkspace(policy, params, principal, headers);
      if (r && "status" in r) { if (mutating) await safeAudit(() => audit(req, { ...auditBase, status: r.status, extra: { denied: "workspace" } })); return r; }
      ws = r;
    }
    if (policy.access === "permission") {
      const d = authorize(policy.permission!, { workspaceId: ws!.id, kind: policy.resource, id: params.id })(principal);
      if (!d.allow) {
        if (mutating) await safeAudit(() => audit(req, { ...auditBase, ws, status: d.status, extra: { denied: d.code, permission: policy.permission } }));
        return errRes(d.status, d.code, d.message);
      }
    }

    let res: EndpointResult;
    try {
      res = policy.action === "workspace.delete" && ws && o.serializeWorkspaceDelete
        ? await o.serializeWorkspaceDelete(ws.id, () => run(ws))
        : await run(ws);
    }
    catch (e) {
      if (e instanceof ApiError && mutating && e.code !== "audit_unavailable") { await safeAudit(() => audit(req, { ...auditBase, ws, status: e.status, extra: { error: e.code } })); }
      throw e;
    }
    if (policy.action === "workspace.delete" && res.status < 300 && ws) await o.onWorkspaceDeleted?.(ws.id);
    if (mutating && !auditWritten) await safeAudit(() => audit(req, { ...auditBase, ws, status: res.status, detail: res.audit }));
    return strip(res);
  }

  async function safeAudit(fn: () => Promise<void>) {
    try { await fn(); }
    catch (e) { o.onAuditError?.(e); throw new ApiError(500, "audit_unavailable", "audit log write failed"); }
  }
}
