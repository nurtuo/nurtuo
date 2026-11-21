// SPDX-License-Identifier: AGPL-3.0-only
import type { Role } from "./permissions.js";
import type { ApiTokenRecord, AuditEntry, AuditFilter, AuditPage, ExternalIdentity, Membership, NewAuditEntry, Session, User, Workspace } from "./types.js";

export type MemberChange = "ok" | "not_found" | "last_owner";

/** Persistence for identity, RBAC, tokens, sessions and the append-only audit log. */
export interface AuthRepository {
  // users
  /** Email is stored lower-cased; duplicates throw ApiError 409. */
  createUser(u: { email: string; displayName: string; passwordHash: string | null }): Promise<User>;
  getUserById(id: string): Promise<User | undefined>;
  getUserByEmail(email: string): Promise<User | undefined>;
  /** Atomically increments the failure counter; locks the account for lockMs once max is reached. */
  recordLoginFailure(userId: string, max: number, lockMs: number, now: Date): Promise<{ failedLogins: number; lockedUntil: string | null }>;
  recordLoginSuccess(userId: string): Promise<void>;
  setUserStatus(userId: string, status: User["status"]): Promise<void>;
  getIdentity(provider: string, subject: string): Promise<ExternalIdentity | undefined>;
  linkIdentity(i: ExternalIdentity): Promise<void>;

  // workspaces + memberships
  /** Creates the workspace and the owner membership in one transaction. Duplicate slug -> ApiError 409. */
  createWorkspace(w: { slug: string; name: string; ownerId: string }): Promise<Workspace>;
  getWorkspace(idOrSlug: string): Promise<Workspace | undefined>;
  updateWorkspace(id: string, patch: { name?: string }): Promise<Workspace | undefined>;
  deleteWorkspace(id: string): Promise<boolean>;
  listMembershipsForUser(userId: string): Promise<Array<Membership & { workspace: Workspace }>>;
  getMembership(workspaceId: string, userId: string): Promise<Membership | undefined>;
  listMembers(workspaceId: string): Promise<Array<Membership & { email: string; displayName: string }>>;
  /** Insert or change role. Demoting the last owner returns "last_owner" (atomic). */
  setMember(workspaceId: string, userId: string, role: Role): Promise<MemberChange>;
  removeMember(workspaceId: string, userId: string): Promise<MemberChange>;

  // api tokens
  createToken(t: Omit<ApiTokenRecord, "id" | "createdAt" | "lastUsedAt" | "revokedAt">): Promise<ApiTokenRecord>;
  getTokenByPrefix(prefix: string): Promise<ApiTokenRecord | undefined>;
  getToken(id: string): Promise<ApiTokenRecord | undefined>;
  listTokens(workspaceId: string, createdBy?: string): Promise<ApiTokenRecord[]>;
  revokeToken(id: string, now: Date): Promise<boolean>;
  touchToken(id: string, now: Date): Promise<void>;

  // sessions
  createSession(s: { userId: string; expiresAt: Date; ip: string | null; userAgent: string | null }): Promise<Session>;
  getSession(id: string): Promise<Session | undefined>;
  revokeSession(id: string, now: Date): Promise<void>;
  revokeSessionsForUser(userId: string, now: Date): Promise<void>;

  // audit (append-only: there is intentionally no update or delete)
  appendAudit(e: NewAuditEntry): Promise<AuditEntry>;
  /** Newest first. `before` is a seq cursor; nextCursor is set when more rows exist. */
  queryAudit(f: AuditFilter): Promise<AuditPage>;
}
