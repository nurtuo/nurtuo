// SPDX-License-Identifier: AGPL-3.0-only
// Route policy table: the authorization decision for every route of the control-plane API.
// A request that matches no policy is denied (fail closed). policy.test.ts fails if any path in
// openapi.yaml has no policy, so adding a route to api.ts without deciding who may call it breaks CI.
import type { Permission, Role } from "./permissions.js";
import { permissionsIn } from "./authz.js";
import type { InstancePermission } from "./instance.js";
import type { Principal } from "./types.js";

export interface RoutePolicy {
  method: string;
  /** "/v1/components/:id"; segments starting with ":" are parameters. */
  pattern: string;
  /**
   * public: no credentials needed (login, SSO callbacks); authenticated: any valid principal;
   * permission: needs workspace `permission`; instance: needs instance-level `instancePermission`.
   */
  access: "public" | "authenticated" | "permission" | "instance";
  permission?: Permission;
  /** For access "instance": the instance-level permission (see instance.ts). */
  instancePermission?: InstancePermission;
  /**
   * Where the workspace comes from. path: the :ws parameter; context: X-Nurtuo-Workspace header,
   * the token's own workspace, or the caller's only membership; none: not workspace-scoped.
   */
  workspace: "path" | "context" | "none";
  /** Session users only; API tokens are refused (e.g. creating workspaces). */
  userOnly?: boolean;
  /** Audit vocabulary: resource kind and action name. */
  resource: string;
  action: string;
  /** For legacy (api.ts) routes: GET path used to capture the before-state hash on mutations. */
  beforePath?: (params: Record<string, string>) => string;
}

const P = (method: string, pattern: string, p: Omit<RoutePolicy, "method" | "pattern">): RoutePolicy => ({ method, pattern, ...p });
const perm = (permission: Permission, resource: string, action: string, extra: Partial<RoutePolicy> = {}): Omit<RoutePolicy, "method" | "pattern"> =>
  ({ access: "permission", permission, workspace: "context", resource, action, ...extra });
const wsPerm = (permission: Permission, resource: string, action: string, extra: Partial<RoutePolicy> = {}) => perm(permission, resource, action, { workspace: "path", ...extra });
const pub = (resource: string, action: string): Omit<RoutePolicy, "method" | "pattern"> => ({ access: "public", workspace: "none", resource, action });
const inst = (instancePermission: InstancePermission, resource: string, action: string, extra: Partial<RoutePolicy> = {}): Omit<RoutePolicy, "method" | "pattern"> =>
  ({ access: "instance", instancePermission, workspace: "none", resource, action, ...extra });
const authed = (resource: string, action: string, extra: Partial<RoutePolicy> = {}): Omit<RoutePolicy, "method" | "pattern"> => ({ access: "authenticated", workspace: "none", resource, action, ...extra });

const compPath = (p: Record<string, string>) => `/v1/components/${p.id}`;

export const POLICIES: readonly RoutePolicy[] = [
  // ---- control-plane resources (implemented in api.ts) ----
  P("GET", "/v1/components", perm("component:read", "component", "component.list")),
  P("POST", "/v1/components", perm("component:write", "component", "component.create")),
  P("GET", "/v1/components/:id", perm("component:read", "component", "component.get")),
  P("PATCH", "/v1/components/:id", perm("component:write", "component", "component.update", { beforePath: compPath })),
  P("DELETE", "/v1/components/:id", perm("component:delete", "component", "component.delete", { beforePath: compPath })),
  P("GET", "/v1/components/:id/revisions", perm("component:read", "component", "revision.list")),
  P("POST", "/v1/components/:id/revisions", perm("component:write", "component", "revision.create")),
  P("GET", "/v1/components/:id/revisions/:number", perm("component:read", "component", "revision.get")),
  P("GET", "/v1/runtimes", perm("runtime:read", "runtime", "runtime.list")),
  P("POST", "/v1/runtimes", perm("runtime:register", "runtime", "runtime.register")),
  P("GET", "/v1/runtimes/:id", perm("runtime:read", "runtime", "runtime.get")),
  P("GET", "/v1/runtimes/:id/desired-state", perm("runtime:read", "runtime", "desired_state.get")),
  P("DELETE", "/v1/runtimes/:id/deployments/:componentId", perm("deployment:create", "deployment", "deployment.undeploy")),
  P("GET", "/v1/deployments", perm("deployment:read", "deployment", "deployment.list")),
  P("POST", "/v1/deployments", perm("deployment:create", "deployment", "deployment.create")),
  P("GET", "/v1/deployments/:id", perm("deployment:read", "deployment", "deployment.get")),
  P("PUT", "/v1/deployments/:id/status", perm("deployment:report", "deployment", "deployment.report_status", { beforePath: (p) => `/v1/deployments/${p.id}` })),

  // ---- authentication ----
  P("POST", "/v1/auth/login", pub("session", "auth.login")),
  P("POST", "/v1/auth/logout", authed("session", "auth.logout")),
  P("GET", "/v1/auth/me", authed("session", "auth.me")),
  P("GET", "/v1/auth/oidc/login", pub("session", "auth.oidc_login")),
  P("GET", "/v1/auth/oidc/callback", pub("session", "auth.oidc_callback")),
  // SAML + SCIM: documented stubs (501), see docs/auth.md. Public because there is nothing to protect yet.
  P("GET", "/v1/auth/saml/login", pub("session", "auth.saml_login")),
  P("POST", "/v1/auth/saml/acs", pub("session", "auth.saml_acs")),
  P("GET", "/v1/auth/saml/metadata", pub("session", "auth.saml_metadata")),
  P("GET", "/v1/scim/v2/Users", pub("user", "scim.users.list")),
  P("POST", "/v1/scim/v2/Users", pub("user", "scim.users.create")),
  P("GET", "/v1/scim/v2/Users/:id", pub("user", "scim.users.get")),
  P("PUT", "/v1/scim/v2/Users/:id", pub("user", "scim.users.replace")),
  P("PATCH", "/v1/scim/v2/Users/:id", pub("user", "scim.users.patch")),
  P("DELETE", "/v1/scim/v2/Users/:id", pub("user", "scim.users.delete")),
  P("GET", "/v1/scim/v2/Groups", pub("group", "scim.groups.list")),
  P("POST", "/v1/scim/v2/Groups", pub("group", "scim.groups.create")),

  // ---- workspaces, members, tokens, audit ----
  P("GET", "/v1/roles", authed("role", "role.list")),
  P("GET", "/v1/workspaces", authed("workspace", "workspace.list", { userOnly: true })),
  P("POST", "/v1/workspaces", inst("instance:workspace_create", "workspace", "workspace.create", { userOnly: true })),
  P("GET", "/v1/workspaces/:ws", wsPerm("workspace:read", "workspace", "workspace.get")),
  P("PATCH", "/v1/workspaces/:ws", wsPerm("workspace:manage", "workspace", "workspace.update")),
  P("DELETE", "/v1/workspaces/:ws", wsPerm("workspace:delete", "workspace", "workspace.delete")),
  P("GET", "/v1/workspaces/:ws/members", wsPerm("member:read", "member", "member.list")),
  P("POST", "/v1/workspaces/:ws/members", wsPerm("member:manage", "member", "member.add")),
  P("PATCH", "/v1/workspaces/:ws/members/:userId", wsPerm("member:manage", "member", "member.update_role")),
  P("DELETE", "/v1/workspaces/:ws/members/:userId", wsPerm("member:manage", "member", "member.remove")),
  P("GET", "/v1/workspaces/:ws/tokens", wsPerm("token:read", "token", "token.list")),
  P("POST", "/v1/workspaces/:ws/tokens", wsPerm("token:create", "token", "token.create")),
  P("DELETE", "/v1/workspaces/:ws/tokens/:tokenId", wsPerm("token:revoke", "token", "token.revoke")),
  P("GET", "/v1/workspaces/:ws/audit", wsPerm("audit:read", "audit", "audit.query")),

  // ---- canvas Run: connections, write-only secrets, test runs (docs/specs/2026-11-23-canvas-run.md) ----
  P("GET", "/v1/workspaces/:ws/connections", wsPerm("connection:read", "connection", "connection.list")),
  P("GET", "/v1/workspaces/:ws/connections/:id", wsPerm("connection:read", "connection", "connection.get")),
  P("PUT", "/v1/workspaces/:ws/connections/:id", wsPerm("connection:write", "connection", "connection.put")),
  P("DELETE", "/v1/workspaces/:ws/connections/:id", wsPerm("connection:delete", "connection", "connection.delete")),
];

export const MUTATING = new Set(["POST", "PUT", "PATCH", "DELETE"]);

const segs = (p: string) => p.replace(/\/+$/, "").split("/").filter(Boolean);

export function matchPolicy(method: string, path: string): { policy: RoutePolicy; params: Record<string, string> } | undefined {
  const s = segs(path);
  const m = method.toUpperCase();
  for (const policy of POLICIES) {
    if (policy.method !== m) continue;
    const ps = segs(policy.pattern);
    if (ps.length !== s.length) continue;
    const params: Record<string, string> = {};
    let ok = true;
    for (let i = 0; i < ps.length; i++) {
      const seg = ps[i]!;
      if (seg.startsWith(":")) { try { params[seg.slice(1)] = decodeURIComponent(s[i]!); } catch { ok = false; break; } }
      else if (seg !== s[i]) { ok = false; break; }
    }
    if (ok) return { policy, params };
  }
  return undefined;
}

export interface ResourceRef { workspaceId: string; kind: string; id?: string }
export type Decision = { allow: true } | { allow: false; status: 401 | 403; code: string; message: string };

/**
 * authorize(permission, resource) returns a guard. The guard allows only when the principal holds
 * `permission` in the resource's workspace, so a principal can never act on a resource of a
 * workspace it does not belong to, regardless of its role elsewhere.
 */
export function authorize(permission: Permission, resource: ResourceRef): (p: Principal | null) => Decision {
  return (p) => {
    if (!p) return { allow: false, status: 401, code: "unauthenticated", message: "authentication required" };
    if (!permissionsIn(p, resource.workspaceId).has(permission)) return { allow: false, status: 403, code: "forbidden", message: `missing permission ${permission}` };
    return { allow: true };
  };
}

export type { Role };
