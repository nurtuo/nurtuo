// SPDX-License-Identifier: AGPL-3.0-only
import { randomUUID } from "node:crypto";
import { ApiError } from "../domain.js";
import type { Role } from "./permissions.js";
import type { AuthRepository, MemberChange } from "./repository.js";
import type { ApiTokenRecord, AuditEntry, AuditFilter, AuditPage, ExternalIdentity, Membership, NewAuditEntry, Session, User, Workspace } from "./types.js";

const now = () => new Date().toISOString();

export class MemoryAuthRepository implements AuthRepository {
  private users = new Map<string, User>();
  private identities = new Map<string, ExternalIdentity>();
  private workspaces = new Map<string, Workspace>();
  private members = new Map<string, Membership>(); // `${ws}/${user}`
  private tokens = new Map<string, ApiTokenRecord>();
  private sessions = new Map<string, Session>();
  private audit: AuditEntry[] = [];

  async createUser(u: { email: string; displayName: string; passwordHash: string | null }) {
    const email = u.email.trim().toLowerCase();
    if (await this.getUserByEmail(email)) throw new ApiError(409, "conflict", "email already registered");
    const user: User = { id: randomUUID(), email, displayName: u.displayName, passwordHash: u.passwordHash, status: "active", failedLogins: 0, lockedUntil: null, createdAt: now() };
    this.users.set(user.id, user);
    return { ...user };
  }
  async getUserById(id: string) { const u = this.users.get(id); return u && { ...u }; }
  async getUserByEmail(email: string) {
    const e = email.trim().toLowerCase();
    for (const u of this.users.values()) if (u.email === e) return { ...u };
    return undefined;
  }
  async recordLoginFailure(userId: string, max: number, lockMs: number, at: Date) {
    const u = this.users.get(userId);
    if (!u) return { failedLogins: 0, lockedUntil: null };
    u.failedLogins += 1;
    if (u.failedLogins >= max) { u.lockedUntil = new Date(at.getTime() + lockMs).toISOString(); u.failedLogins = 0; }
    return { failedLogins: u.failedLogins, lockedUntil: u.lockedUntil };
  }
  async recordLoginSuccess(userId: string) { const u = this.users.get(userId); if (u) { u.failedLogins = 0; u.lockedUntil = null; } }
  async setUserStatus(userId: string, status: User["status"]) { const u = this.users.get(userId); if (u) u.status = status; }
  async getIdentity(provider: string, subject: string) { return this.identities.get(`${provider}\u0000${subject}`); }
  async linkIdentity(i: ExternalIdentity) { this.identities.set(`${i.provider}\u0000${i.subject}`, i); }

  async createWorkspace(w: { slug: string; name: string; ownerId: string }) {
    for (const x of this.workspaces.values()) if (x.slug === w.slug) throw new ApiError(409, "conflict", `workspace ${w.slug} exists`);
    const ws: Workspace = { id: randomUUID(), slug: w.slug, name: w.name, createdAt: now() };
    this.workspaces.set(ws.id, ws);
    this.members.set(`${ws.id}/${w.ownerId}`, { workspaceId: ws.id, userId: w.ownerId, role: "owner", createdAt: now() });
    return { ...ws };
  }
  async getWorkspace(idOrSlug: string) {
    const direct = this.workspaces.get(idOrSlug);
    if (direct) return { ...direct };
    for (const w of this.workspaces.values()) if (w.slug === idOrSlug) return { ...w };
    return undefined;
  }
  async updateWorkspace(id: string, patch: { name?: string }) { const w = this.workspaces.get(id); if (!w) return undefined; if (patch.name !== undefined) w.name = patch.name; return { ...w }; }
  async deleteWorkspace(id: string) {
    if (!this.workspaces.delete(id)) return false;
    for (const k of [...this.members.keys()]) if (k.startsWith(`${id}/`)) this.members.delete(k);
    for (const [k, t] of this.tokens) if (t.workspaceId === id) this.tokens.delete(k);
    return true;
  }
  async listMembershipsForUser(userId: string) {
    return [...this.members.values()].filter((m) => m.userId === userId).map((m) => ({ ...m, workspace: { ...this.workspaces.get(m.workspaceId)! } }));
  }
  async getMembership(workspaceId: string, userId: string) { const m = this.members.get(`${workspaceId}/${userId}`); return m && { ...m }; }
  async listMembers(workspaceId: string) {
    return [...this.members.values()].filter((m) => m.workspaceId === workspaceId).map((m) => ({ ...m, email: this.users.get(m.userId)?.email ?? "", displayName: this.users.get(m.userId)?.displayName ?? "" }));
  }
  private owners(ws: string) { return [...this.members.values()].filter((m) => m.workspaceId === ws && m.role === "owner").length; }
  async setMember(workspaceId: string, userId: string, role: Role): Promise<MemberChange> {
    const k = `${workspaceId}/${userId}`;
    const cur = this.members.get(k);
    if (cur?.role === "owner" && role !== "owner" && this.owners(workspaceId) <= 1) return "last_owner";
    this.members.set(k, { workspaceId, userId, role, createdAt: cur?.createdAt ?? now() });
    return "ok";
  }
  async removeMember(workspaceId: string, userId: string): Promise<MemberChange> {
    const k = `${workspaceId}/${userId}`;
    const cur = this.members.get(k);
    if (!cur) return "not_found";
    if (cur.role === "owner" && this.owners(workspaceId) <= 1) return "last_owner";
    this.members.delete(k);
    for (const t of this.tokens.values()) if (t.workspaceId === workspaceId && t.createdBy === userId && !t.revokedAt) t.revokedAt = now();
    return "ok";
  }

  async createToken(t: Omit<ApiTokenRecord, "id" | "createdAt" | "lastUsedAt" | "revokedAt">) {
    for (const x of this.tokens.values()) if (x.prefix === t.prefix) throw new ApiError(409, "conflict", "token prefix collision");
    const rec: ApiTokenRecord = { ...t, id: randomUUID(), createdAt: now(), lastUsedAt: null, revokedAt: null };
    this.tokens.set(rec.id, rec);
    return { ...rec };
  }
  async getTokenByPrefix(prefix: string) { for (const t of this.tokens.values()) if (t.prefix === prefix) return { ...t }; return undefined; }
  async getToken(id: string) { const t = this.tokens.get(id); return t && { ...t }; }
  async listTokens(workspaceId: string, createdBy?: string) {
    return [...this.tokens.values()].filter((t) => t.workspaceId === workspaceId && (!createdBy || t.createdBy === createdBy)).map((t) => ({ ...t }));
  }
  async revokeToken(id: string, at: Date) { const t = this.tokens.get(id); if (!t || t.revokedAt) return false; t.revokedAt = at.toISOString(); return true; }
  async touchToken(id: string, at: Date) { const t = this.tokens.get(id); if (t) t.lastUsedAt = at.toISOString(); }

  async createSession(s: { userId: string; expiresAt: Date; ip: string | null; userAgent: string | null }) {
    const row: Session = { id: randomUUID(), userId: s.userId, createdAt: now(), expiresAt: s.expiresAt.toISOString(), revokedAt: null, ip: s.ip, userAgent: s.userAgent };
    this.sessions.set(row.id, row);
    return { ...row };
  }
  async getSession(id: string) { const s = this.sessions.get(id); return s && { ...s }; }
  async revokeSession(id: string, at: Date) { const s = this.sessions.get(id); if (s && !s.revokedAt) s.revokedAt = at.toISOString(); }
  async revokeSessionsForUser(userId: string, at: Date) { for (const s of this.sessions.values()) if (s.userId === userId && !s.revokedAt) s.revokedAt = at.toISOString(); }

  async appendAudit(e: NewAuditEntry) {
    const entry: AuditEntry = Object.freeze({ ...e, ts: e.ts ?? now(), id: randomUUID(), seq: this.audit.length + 1 }) as AuditEntry;
    this.audit.push(entry);
    return entry;
  }
  async queryAudit(f: AuditFilter): Promise<AuditPage> {
    const rows = this.audit.filter((e) =>
      e.workspaceId === f.workspaceId &&
      (f.before === undefined || e.seq < f.before) &&
      (!f.actorId || e.actorId === f.actorId) && (!f.action || e.action === f.action) &&
      (!f.resource || e.resource === f.resource) && (!f.resourceId || e.resourceId === f.resourceId) &&
      (!f.from || e.ts >= f.from) && (!f.to || e.ts < f.to) && (f.status === undefined || e.status === f.status),
    ).sort((a, b) => b.seq - a.seq);
    const items = rows.slice(0, f.limit);
    return { items, nextCursor: rows.length > f.limit ? String(items[items.length - 1]!.seq) : null };
  }
}
