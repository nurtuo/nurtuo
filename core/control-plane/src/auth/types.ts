// SPDX-License-Identifier: AGPL-3.0-only
import type { Permission, Role } from "./permissions.js";

export interface User {
  id: string; email: string; displayName: string;
  /** null for SSO-only accounts. */
  passwordHash: string | null;
  status: "active" | "disabled";
  failedLogins: number; lockedUntil: string | null; createdAt: string;
}
export type PublicUser = Pick<User, "id" | "email" | "displayName" | "status" | "createdAt">;
export const publicUser = (u: User): PublicUser => ({ id: u.id, email: u.email, displayName: u.displayName, status: u.status, createdAt: u.createdAt });

export interface ExternalIdentity { provider: string; subject: string; userId: string }
export interface Workspace { id: string; slug: string; name: string; createdAt: string }
export interface Membership { workspaceId: string; userId: string; role: Role; createdAt: string }

export interface ApiTokenRecord {
  id: string; workspaceId: string; createdBy: string; name: string;
  /** Public lookup part of the token; not secret. */
  prefix: string;
  /** sha256 hex of the secret part. The full token is shown once and never stored. */
  hash: string;
  scopes: Permission[];
  expiresAt: string | null; lastUsedAt: string | null; revokedAt: string | null; createdAt: string;
}
export type PublicToken = Omit<ApiTokenRecord, "hash">;
export const publicToken = ({ hash: _h, ...rest }: ApiTokenRecord): PublicToken => rest;

export interface Session { id: string; userId: string; createdAt: string; expiresAt: string; revokedAt: string | null; ip: string | null; userAgent: string | null }

export interface AuditEntry {
  id: string; seq: number; ts: string;
  workspaceId: string | null;
  actorType: "user" | "token" | "anonymous"; actorId: string | null;
  action: string; resource: string; resourceId: string | null;
  beforeHash: string | null; afterHash: string | null;
  ip: string | null; requestId: string; status: number;
  metadata: Record<string, unknown>;
}
export type NewAuditEntry = Omit<AuditEntry, "id" | "seq" | "ts"> & { ts?: string };
export interface AuditFilter {
  workspaceId: string; actorId?: string; action?: string; resource?: string; resourceId?: string;
  from?: string; to?: string; status?: number; limit: number; before?: number;
}
export interface AuditPage { items: AuditEntry[]; nextCursor: string | null }

/**
 * The authenticated caller. Effective permissions in a workspace come from authz.permissionsIn():
 * the creator's current role in that workspace, intersected with token scopes for API tokens.
 */
export interface Principal {
  kind: "user" | "token";
  userId: string;
  /** Fixed workspace for tokens; null for sessions (resolved per request). */
  tokenWorkspaceId: string | null;
  tokenId?: string; sessionId?: string; csrf?: string;
  /** null for sessions. */
  tokenScopes: ReadonlySet<Permission> | null;
  tokenExpiresAt?: string | null;
  /** workspace -> role. For tokens this holds only the token's workspace (creator's current role). */
  memberships: ReadonlyMap<string, Role>;
  viaCookie: boolean;
}
