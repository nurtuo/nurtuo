// SPDX-License-Identifier: AGPL-3.0-only
// Handlers for the routes owned by the auth module. They run only after authn + authz succeeded
// (or for public routes), and return optional audit details so secrets never reach the audit log.
import { ApiError } from "../domain.js";
import type { ApiRequest, ApiResponse } from "../api.js";
import { sha256hex } from "./crypto.js";
import { enterpriseStubs } from "./stub-routes.js";
import type { OidcProvider } from "./oidc.js";
import { ALL_PERMISSIONS, ROLES, ROLE_DESCRIPTIONS, ROLE_PERMISSIONS, canGrant, isRole, type Role } from "./permissions.js";
import { permissionsIn } from "./authz.js";
import { RateLimited, parseCookies, type AuthService } from "./service.js";
import { publicToken, publicUser, type Principal, type Workspace } from "./types.js";

export interface OidcSetup {
  provider: OidcProvider; issuer: string;
  /** Just-in-time provisioning: unknown identities become members of this workspace with this role (never owner/admin). */
  jit?: { workspace: string; role: Role };
  /** Link an unknown IdP identity to an existing local account with the same *verified* email. Off by default (account takeover risk if the IdP is lax). */
  linkByVerifiedEmail?: boolean;
}

export interface AuditDetail { resourceId?: string | null; before?: unknown; after?: unknown; metadata?: Record<string, unknown>; actor?: { type: "user" | "token" | "anonymous"; id: string | null }; workspaceId?: string | null }
export type EndpointResult = ApiResponse & { audit?: AuditDetail };

export interface EndpointCtx {
  req: ApiRequest; principal: Principal | null; params: Record<string, string>; query: Record<string, string>;
  workspace?: Workspace; ip: string | null; requestId: string; service: AuthService; oidc?: OidcSetup;
  /** Aborted when the HTTP client disconnects before the response is sent. */
  signal?: AbortSignal;
}
export type Endpoint = (c: EndpointCtx) => Promise<EndpointResult>;

const bad = (m: string) => new ApiError(400, "bad_request", m);
const forbidden = (code: string, m: string) => new ApiError(403, code, m);
const obj = (b: unknown): Record<string, unknown> => { if (!b || typeof b !== "object" || Array.isArray(b)) throw bad("body must be a JSON object"); return b as Record<string, unknown>; };
const need = (p: Principal | null): Principal => { if (!p) throw new ApiError(401, "unauthenticated", "authentication required"); return p; };

const SLUG = /^[a-z0-9][a-z0-9-]{1,62}$/;
const UUIDISH = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const ua = (r: ApiRequest) => r.headers?.["user-agent"] ?? null;

function sessionResponse(c: EndpointCtx, s: { jwt: string; csrf: string; expiresAt: string }, user: Parameters<typeof publicUser>[0], extraCookie?: string): ApiResponse {
  const cookies = [c.service.sessionCookie(s.jwt)];
  if (extraCookie) cookies.push(extraCookie);
  return { status: 200, headers: { "set-cookie": cookies }, body: { user: publicUser(user), csrfToken: s.csrf, expiresAt: s.expiresAt } };
}

export const endpoints: Record<string, Endpoint> = {
  async "auth.login"(c) {
    const b = obj(c.req.body);
    const emailHash = typeof b.email === "string" ? sha256hex(b.email.trim().toLowerCase()).slice(0, 16) : null;
    try {
      const r = await c.service.login(b.email, b.password, c.ip, ua(c.req));
      return { ...sessionResponse(c, r, r.user), audit: { actor: { type: "user", id: r.user.id }, resourceId: r.session.id, metadata: { outcome: "success" } } };
    } catch (e) {
      if (e instanceof ApiError) {
        const res: EndpointResult = { status: e.status, body: { error: { code: e.code, message: e.message } }, audit: { metadata: { outcome: "failure", code: e.code, emailSha256: emailHash } } };
        if (e instanceof RateLimited) res.headers = { "retry-after": String(e.retryAfterSec) };
        return res;
      }
      throw e;
    }
  },

  async "auth.logout"(c) {
    const p = need(c.principal);
    if (!p.sessionId) throw bad("logout applies to browser sessions; revoke API tokens via DELETE /v1/workspaces/{ws}/tokens/{id}");
    await c.service.repo.revokeSession(p.sessionId, c.service.now());
    return { status: 204, headers: { "set-cookie": c.service.clearCookie() }, audit: { resourceId: p.sessionId } };
  },

  async "auth.me"(c) {
    const p = need(c.principal);
    const user = await c.service.repo.getUserById(p.userId);
    const ms = (await c.service.repo.listMembershipsForUser(p.userId)).filter((m) => p.kind === "user" || m.workspaceId === p.tokenWorkspaceId);
    return {
      status: 200,
      body: {
        user: user && publicUser(user), authType: p.kind,
        csrfToken: p.csrf,
        token: p.kind === "token" ? { id: p.tokenId, workspaceId: p.tokenWorkspaceId, scopes: [...(p.tokenScopes ?? [])], expiresAt: p.tokenExpiresAt } : undefined,
        workspaces: ms.map((m) => ({ id: m.workspaceId, slug: m.workspace.slug, name: m.workspace.name, role: m.role, permissions: [...permissionsIn(p, m.workspaceId)] })),
      },
    };
  },

  async "auth.oidc_login"(c) {
    if (!c.oidc) return { status: 404, body: { error: { code: "not_found", message: "OIDC is not configured" } } };
    const b = await c.oidc.provider.begin();
    const cookie = `__Host-nurtuo_oidc=${c.service.signState({ state: b.state, nonce: b.nonce, cv: b.codeVerifier }, 600)}; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=600`;
    return { status: 302, headers: { location: b.authorizationUrl, "set-cookie": cookie } };
  },

  async "auth.oidc_callback"(c) {
    if (!c.oidc) return { status: 404, body: { error: { code: "not_found", message: "OIDC is not configured" } } };
    const wait = c.service.limitIp(c.ip);
    if (wait) throw new RateLimited(wait);
    const { code, state } = c.query;
    const raw = parseCookies(c.req.headers?.["cookie"])["__Host-nurtuo_oidc"];
    const st = raw ? c.service.verifyState(raw) : null;
    if (!code || !state || !st) throw new ApiError(401, "oidc_failed", "missing or invalid OIDC state");
    let ident;
    try { ident = await c.oidc.provider.complete({ code, state, expectedState: String(st.state), nonce: String(st.nonce), codeVerifier: String(st.cv) }); }
    catch (e) { return { status: 401, body: { error: { code: "oidc_failed", message: e instanceof ApiError ? e.message : "OIDC login failed" } }, headers: { "set-cookie": clearOidcCookie() }, audit: { metadata: { outcome: "failure", provider: "oidc" } } }; }
    const repo = c.service.repo;
    const provider = `oidc:${ident.issuer}`;
    let userId = (await repo.getIdentity(provider, ident.subject))?.userId;
    if (!userId && ident.email && ident.emailVerified) {
      const existing = await repo.getUserByEmail(ident.email);
      if (existing && c.oidc.linkByVerifiedEmail) { await repo.linkIdentity({ provider, subject: ident.subject, userId: existing.id }); userId = existing.id; }
      else if (!existing && c.oidc.jit) {
        const w = await repo.getWorkspace(c.oidc.jit.workspace);
        if (w) {
          const u = await repo.createUser({ email: ident.email, displayName: ident.name ?? "", passwordHash: null });
          await repo.linkIdentity({ provider, subject: ident.subject, userId: u.id });
          await repo.setMember(w.id, u.id, c.oidc.jit.role);
          userId = u.id;
        }
      }
    }
    const user = userId ? await repo.getUserById(userId) : undefined;
    if (!user || user.status !== "active") return { status: 403, body: { error: { code: "sso_not_provisioned", message: "no active account is linked to this identity" } }, headers: { "set-cookie": clearOidcCookie() }, audit: { metadata: { outcome: "denied", provider: "oidc" } } };
    await repo.recordLoginSuccess(user.id);
    const s = await c.service.startSession(user, c.ip, ua(c.req));
    return { ...sessionResponse(c, s, user, clearOidcCookie()), audit: { actor: { type: "user", id: user.id }, resourceId: s.session.id, metadata: { outcome: "success", provider: "oidc" } } };
  },

  async "role.list"() {
    return { status: 200, body: { permissions: ALL_PERMISSIONS, roles: ROLES.map((r) => ({ name: r, description: ROLE_DESCRIPTIONS[r], permissions: ROLE_PERMISSIONS[r] })) } };
  },

  async "workspace.list"(c) {
    const p = need(c.principal);
    return { status: 200, body: { items: (await c.service.repo.listMembershipsForUser(p.userId)).map((m) => ({ ...m.workspace, role: m.role })) } };
  },

  async "workspace.create"(c) {
    const p = need(c.principal);
    const b = obj(c.req.body);
    if (typeof b.slug !== "string" || !SLUG.test(b.slug) || UUIDISH.test(b.slug)) throw bad("slug must match ^[a-z0-9][a-z0-9-]{1,62}$ and must not look like a UUID");
    if (typeof b.name !== "string" || !b.name.trim() || b.name.length > 100) throw bad("name must be a non-empty string up to 100 chars");
    const w = await c.service.repo.createWorkspace({ slug: b.slug, name: b.name.trim(), ownerId: p.userId });
    return { status: 201, body: w, audit: { resourceId: w.id, workspaceId: w.id, after: w } };
  },

  async "workspace.get"(c) { return { status: 200, body: c.workspace } },

  async "workspace.update"(c) {
    const b = obj(c.req.body);
    if (typeof b.name !== "string" || !b.name.trim() || b.name.length > 100) throw bad("name must be a non-empty string up to 100 chars");
    const w = (await c.service.repo.updateWorkspace(c.workspace!.id, { name: b.name.trim() }))!;
    return { status: 200, body: w, audit: { resourceId: w.id, before: c.workspace, after: w } };
  },

  async "workspace.delete"(c) {
    await c.service.repo.deleteWorkspace(c.workspace!.id);
    return { status: 204, audit: { resourceId: c.workspace!.id, before: c.workspace } };
  },

  async "member.list"(c) {
    return { status: 200, body: { items: await c.service.repo.listMembers(c.workspace!.id) } };
  },

  async "member.add"(c) {
    const p = need(c.principal), w = c.workspace!;
    const b = obj(c.req.body);
    if (!isRole(b.role)) throw bad(`role must be one of ${ROLES.join(", ")}`);
    const held = permissionsIn(p, w.id);
    if (!canGrant(held, b.role)) throw forbidden("role_escalation", `you cannot grant role ${b.role}`);
    const email = c.service.validateEmail(b.email);
    let user = await c.service.repo.getUserByEmail(email);
    if (!user) {
      // New local account; the password is set by the admin and never changed for an existing account here.
      user = await c.service.createPasswordUser(email, b.password as string, typeof b.displayName === "string" ? b.displayName.slice(0, 100) : "");
    }
    const existing = await c.service.repo.getMembership(w.id, user.id);
    if (existing && !canGrant(held, existing.role)) throw forbidden("role_escalation", `you cannot modify a ${existing.role}`);
    const r = await c.service.repo.setMember(w.id, user.id, b.role);
    if (r === "last_owner") throw new ApiError(409, "last_owner", "cannot demote the last owner");
    return { status: existing ? 200 : 201, body: { workspaceId: w.id, userId: user.id, email: user.email, role: b.role }, audit: { resourceId: user.id, before: existing ? { role: existing.role } : null, after: { role: b.role } } };
  },

  async "member.update_role"(c) {
    const p = need(c.principal), w = c.workspace!;
    const b = obj(c.req.body);
    if (!isRole(b.role)) throw bad(`role must be one of ${ROLES.join(", ")}`);
    const held = permissionsIn(p, w.id);
    const cur = await c.service.repo.getMembership(w.id, c.params.userId!);
    if (!cur) throw new ApiError(404, "not_found", "member not found");
    if (!canGrant(held, cur.role) || !canGrant(held, b.role)) throw forbidden("role_escalation", "you cannot assign or change a role above your own permissions");
    if ((await c.service.repo.setMember(w.id, cur.userId, b.role)) === "last_owner") throw new ApiError(409, "last_owner", "cannot demote the last owner");
    return { status: 200, body: { workspaceId: w.id, userId: cur.userId, role: b.role }, audit: { resourceId: cur.userId, before: { role: cur.role }, after: { role: b.role } } };
  },

  async "member.remove"(c) {
    const p = need(c.principal), w = c.workspace!;
    const cur = await c.service.repo.getMembership(w.id, c.params.userId!);
    if (!cur) throw new ApiError(404, "not_found", "member not found");
    if (!canGrant(permissionsIn(p, w.id), cur.role)) throw forbidden("role_escalation", `you cannot remove a ${cur.role}`);
    if ((await c.service.repo.removeMember(w.id, cur.userId)) === "last_owner") throw new ApiError(409, "last_owner", "cannot remove the last owner");
    return { status: 204, audit: { resourceId: cur.userId, before: { role: cur.role } } };
  },

  async "token.list"(c) {
    const p = need(c.principal), w = c.workspace!;
    const all = permissionsIn(p, w.id).has("token:revoke_any");
    return { status: 200, body: { items: (await c.service.repo.listTokens(w.id, all ? undefined : p.userId)).map(publicToken) } };
  },

  async "token.create"(c) {
    const p = need(c.principal), w = c.workspace!;
    const b = obj(c.req.body);
    const { record, token } = await c.service.mintToken({ workspaceId: w.id, creator: p, effective: permissionsIn(p, w.id), name: b.name, scopes: b.scopes, expiresInSec: b.expiresInSeconds });
    // The plaintext token is returned exactly once and is never stored or audited.
    return { status: 201, body: { ...publicToken(record), token }, audit: { resourceId: record.id, after: publicToken(record) } };
  },

  async "token.revoke"(c) {
    const p = need(c.principal), w = c.workspace!;
    const t = await c.service.repo.getToken(c.params.tokenId!);
    if (!t || t.workspaceId !== w.id) throw new ApiError(404, "not_found", "token not found");
    if (t.createdBy !== p.userId && !permissionsIn(p, w.id).has("token:revoke_any")) throw forbidden("forbidden", "missing permission token:revoke_any");
    await c.service.repo.revokeToken(t.id, c.service.now());
    return { status: 204, audit: { resourceId: t.id, before: publicToken(t), after: { revoked: true } } };
  },

  async "audit.query"(c) {
    const q = c.query;
    const limit = q.limit === undefined ? 50 : Number(q.limit);
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) throw bad("limit must be an integer between 1 and 200");
    const date = (k: string) => {
      const v = q[k];
      if (v === undefined) return undefined;
      const t = Date.parse(v);
      if (Number.isNaN(t)) throw bad(`${k} must be an ISO-8601 timestamp`);
      return new Date(t).toISOString();
    };
    if (q.cursor !== undefined && !/^\d{1,15}$/.test(q.cursor)) throw bad("invalid cursor");
    if (q.status !== undefined && !/^\d{3}$/.test(q.status)) throw bad("status must be an HTTP status code");
    const page = await c.service.repo.queryAudit({
      workspaceId: c.workspace!.id, actorId: q.actor, action: q.action, resource: q.resource, resourceId: q.resourceId,
      from: date("from"), to: date("to"), status: q.status ? Number(q.status) : undefined, limit, before: q.cursor ? Number(q.cursor) : undefined,
    });
    return { status: 200, body: page };
  },

  ...enterpriseStubs,
};

const clearOidcCookie = () => "__Host-nurtuo_oidc=; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=0";
