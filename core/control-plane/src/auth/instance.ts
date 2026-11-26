// SPDX-License-Identifier: AGPL-3.0-only
// Instance-level permissions: held by the caller as a person (by email), not through a workspace
// role. Deliberately kept outside RESOURCES/ACTIONS in permissions.ts, so they never reach workspace
// owners through ALL_PERMISSIONS, never become API-token scopes and never appear in GET /v1/roles.
import { isEmailSyntax } from "./types.js";

export const INSTANCE_PERMISSIONS = ["instance:workspace_create"] as const;
export type InstancePermission = (typeof INSTANCE_PERMISSIONS)[number];
export type WorkspaceCreateMode = "admins" | "any";
export interface InstanceConfig {
  /** Lower-case emails. */
  admins: ReadonlySet<string>;
  workspaceCreate: WorkspaceCreateMode;
}

export class InstanceConfigError extends Error {
  constructor(message: string) { super(message); this.name = "InstanceConfigError"; }
}

/** Fail closed: no instance admins, workspace creation for admins only. */
export const DEFAULT_INSTANCE_CONFIG: InstanceConfig = Object.freeze({ admins: new Set<string>(), workspaceCreate: "admins" as const });

const NONE: ReadonlySet<InstancePermission> = new Set();
const ALL: ReadonlySet<InstancePermission> = new Set(INSTANCE_PERMISSIONS);

/**
 * Reads NURTUO_INSTANCE_ADMINS, NURTUO_BOOTSTRAP_EMAIL and NURTUO_ALLOW_WORKSPACE_CREATE.
 * Throws InstanceConfigError on a malformed email or an unknown mode.
 */
export function parseInstanceConfig(env: Record<string, string | undefined>): InstanceConfig {
  const admins = new Set<string>();
  // TODO(#77): temporary phase-1 env-var instance-admin list; replace with DB-backed instance roles.
  const items = (env.NURTUO_INSTANCE_ADMINS ?? "").split(",");
  let pos = 0;
  for (const raw of items) {
    const item = raw.trim();
    if (!item) continue;
    pos++;
    if (!isEmailSyntax(item)) throw new InstanceConfigError(`NURTUO_INSTANCE_ADMINS item ${pos} is not a valid email`);
    admins.add(item.toLowerCase());
  }
  const boot = (env.NURTUO_BOOTSTRAP_EMAIL ?? "").trim();
  if (boot) {
    if (!isEmailSyntax(boot)) throw new InstanceConfigError("NURTUO_BOOTSTRAP_EMAIL is not a valid email");
    admins.add(boot.toLowerCase());
  }
  const mode = (env.NURTUO_ALLOW_WORKSPACE_CREATE ?? "").trim().toLowerCase();
  if (mode !== "" && mode !== "admins" && mode !== "any") throw new InstanceConfigError(`NURTUO_ALLOW_WORKSPACE_CREATE must be "admins" or "any"`);
  return { admins, workspaceCreate: mode === "any" ? "any" : "admins" };
}

/** Instance permissions of a user with `email` (undefined: unknown user). */
export function instancePermissionsFor(email: string | undefined, cfg: InstanceConfig): ReadonlySet<InstancePermission> {
  if (cfg.workspaceCreate === "any") return ALL;
  // TODO(#77): temporary phase-1 env-var instance-admin check; replace with DB-backed instance roles.
  return email !== undefined && cfg.admins.has(email.trim().toLowerCase()) ? ALL : NONE;
}
