// SPDX-License-Identifier: AGPL-3.0-only
import { permissionsOf, type Permission } from "./permissions.js";
import type { Principal } from "./types.js";

const EMPTY: ReadonlySet<Permission> = new Set();

/**
 * Effective permissions of a principal inside one workspace: the role of the (creator) user there,
 * intersected with the token scopes for API tokens. A token is bound to exactly one workspace.
 */
export function permissionsIn(p: Principal, workspaceId: string): ReadonlySet<Permission> {
  const role = p.memberships.get(workspaceId);
  if (!role) return EMPTY;
  const perms = permissionsOf(role);
  if (p.kind === "user") return perms;
  if (p.tokenWorkspaceId !== workspaceId || !p.tokenScopes) return EMPTY;
  return new Set([...perms].filter((x) => p.tokenScopes!.has(x)));
}
