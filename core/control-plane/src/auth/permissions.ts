// SPDX-License-Identifier: AGPL-3.0-only
// Permissions matrix (resource x action) and the built-in roles. This file is the single source
// of truth; migrations/0103_auth_seed_roles.sql mirrors it as reference data and a test fails on drift.
// Everything here is free (no feature gating): all roles ship in the AGPL core.

export const RESOURCES = ["component", "runtime", "deployment", "workspace", "member", "token", "audit"] as const;
export type Resource = (typeof RESOURCES)[number];

export const ACTIONS: Record<Resource, readonly string[]> = {
  component: ["read", "write", "delete"],
  runtime: ["read", "register"],
  deployment: ["read", "create", "report"],
  workspace: ["read", "manage", "delete"],
  member: ["read", "manage"],
  token: ["create", "read", "revoke", "revoke_any"],
  audit: ["read"],
};

export type Permission = `${Resource}:${string}`;
export const ALL_PERMISSIONS: readonly Permission[] = RESOURCES.flatMap((r) => ACTIONS[r].map((a) => `${r}:${a}` as Permission));
const PERM_SET = new Set<string>(ALL_PERMISSIONS);
export const isPermission = (s: unknown): s is Permission => typeof s === "string" && PERM_SET.has(s);

export const ROLES = ["owner", "admin", "editor", "viewer", "deployer", "runtime"] as const;
export type Role = (typeof ROLES)[number];
export const isRole = (s: unknown): s is Role => typeof s === "string" && (ROLES as readonly string[]).includes(s);

const VIEW: Permission[] = ["component:read", "runtime:read", "deployment:read", "workspace:read", "member:read"];
const OWN_TOKENS: Permission[] = ["token:create", "token:read", "token:revoke"];

export const ROLE_PERMISSIONS: Record<Role, readonly Permission[]> = {
  owner: ALL_PERMISSIONS,
  // admin: everything except deleting the workspace (=> cannot grant or touch owners, see canGrant).
  admin: ALL_PERMISSIONS.filter((p) => p !== "workspace:delete"),
  editor: [...VIEW, "component:write", "component:delete", ...OWN_TOKENS],
  viewer: VIEW,
  deployer: [...VIEW, "deployment:create", ...OWN_TOKENS],
  // machine role: a runtime/gateway registering itself, reading desired state and reporting status.
  runtime: ["component:read", "runtime:read", "runtime:register", "deployment:read", "deployment:report"],
};

export const ROLE_DESCRIPTIONS: Record<Role, string> = {
  owner: "Full control including deleting the workspace and managing owners",
  admin: "Manage members, tokens, components and deployments; cannot delete the workspace or grant owner",
  editor: "Create and edit components and revisions",
  viewer: "Read-only access",
  deployer: "Read-only plus create deployments",
  runtime: "Machine role for runtimes and gateways",
};

export function permissionsOf(role: Role): ReadonlySet<Permission> { return new Set(ROLE_PERMISSIONS[role]); }

/**
 * Privilege-escalation rule: a principal may grant (or modify/remove a holder of) a role only if
 * it holds every permission of that role. Role-agnostic, so admin can never mint an owner and an
 * editor can never mint anything above editor.
 */
export function canGrant(held: ReadonlySet<Permission>, target: Role): boolean {
  return ROLE_PERMISSIONS[target].every((p) => held.has(p));
}
