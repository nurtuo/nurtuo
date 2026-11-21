// SPDX-License-Identifier: AGPL-3.0-only
import { randomUUID } from "node:crypto";
import { ApiError } from "../domain.js";
import type { Db, Queryable } from "../pg-repository.js";
import type { Permission, Role } from "./permissions.js";
import type { AuthRepository, MemberChange } from "./repository.js";
import type { ApiTokenRecord, AuditEntry, AuditFilter, AuditPage, ExternalIdentity, Membership, NewAuditEntry, Session, User, Workspace } from "./types.js";

const iso = (v: unknown) => (v instanceof Date ? v.toISOString() : (v as string));
const isoN = (v: unknown) => (v == null ? null : iso(v));
const user = (r: any): User => ({ id: r.id, email: r.email, displayName: r.display_name, passwordHash: r.password_hash, status: r.status, failedLogins: r.failed_logins, lockedUntil: isoN(r.locked_until), createdAt: iso(r.created_at) });
const ws = (r: any): Workspace => ({ id: r.id, slug: r.slug, name: r.name, createdAt: iso(r.created_at) });
const mem = (r: any): Membership => ({ workspaceId: r.workspace_id, userId: r.user_id, role: r.role as Role, createdAt: iso(r.created_at) });
const tok = (r: any): ApiTokenRecord => ({ id: r.id, workspaceId: r.workspace_id, createdBy: r.created_by, name: r.name, prefix: r.prefix, hash: r.hash, scopes: r.scopes as Permission[], expiresAt: isoN(r.expires_at), lastUsedAt: isoN(r.last_used_at), revokedAt: isoN(r.revoked_at), createdAt: iso(r.created_at) });
const ses = (r: any): Session => ({ id: r.id, userId: r.user_id, createdAt: iso(r.created_at), expiresAt: iso(r.expires_at), revokedAt: isoN(r.revoked_at), ip: r.ip, userAgent: r.user_agent });
const aud = (r: any): AuditEntry => ({ id: r.id, seq: Number(r.seq), ts: iso(r.ts), workspaceId: r.workspace_id, actorType: r.actor_type, actorId: r.actor_id, action: r.action, resource: r.resource, resourceId: r.resource_id, beforeHash: r.before_hash, afterHash: r.after_hash, ip: r.ip, requestId: r.request_id, status: r.status, metadata: r.metadata });

export class PgAuthRepository implements AuthRepository {
  constructor(private db: Db) {}

  private async tx<T>(fn: (q: Queryable) => Promise<T>): Promise<T> {
    const c = await this.db.connect();
    try { await c.query("BEGIN"); const v = await fn(c); await c.query("COMMIT"); return v; }
    catch (e) { await c.query("ROLLBACK"); throw e; } finally { c.release(); }
  }
  private static dup(e: any, msg: string): never {
    if (e?.code === "23505") throw new ApiError(409, "conflict", msg);
    throw e;
  }

  async createUser(u: { email: string; displayName: string; passwordHash: string | null }) {
    try {
      const r = await this.db.query("INSERT INTO users(id,email,display_name,password_hash) VALUES ($1,$2,$3,$4) RETURNING *", [randomUUID(), u.email.trim().toLowerCase(), u.displayName, u.passwordHash]);
      return user(r.rows[0]);
    } catch (e) { return PgAuthRepository.dup(e, "email already registered"); }
  }
  async getUserById(id: string) { const r = await this.db.query("SELECT * FROM users WHERE id=$1", [id]); return r.rows[0] ? user(r.rows[0]) : undefined; }
  async getUserByEmail(email: string) { const r = await this.db.query("SELECT * FROM users WHERE email=$1", [email.trim().toLowerCase()]); return r.rows[0] ? user(r.rows[0]) : undefined; }
  async recordLoginFailure(userId: string, max: number, lockMs: number, now: Date) {
    // Single statement: increments atomically so concurrent guesses cannot dodge the counter.
    const r = await this.db.query(
      `UPDATE users SET
         failed_logins = CASE WHEN failed_logins + 1 >= $2::int THEN 0 ELSE failed_logins + 1 END,
         locked_until  = CASE WHEN failed_logins + 1 >= $2::int THEN $4::timestamptz + ($3::int * interval '1 millisecond') ELSE locked_until END
       WHERE id=$1 RETURNING failed_logins, locked_until`, [userId, max, lockMs, now.toISOString()]);
    const row = r.rows[0];
    return row ? { failedLogins: row.failed_logins as number, lockedUntil: isoN(row.locked_until) } : { failedLogins: 0, lockedUntil: null };
  }
  async recordLoginSuccess(userId: string) { await this.db.query("UPDATE users SET failed_logins=0, locked_until=NULL WHERE id=$1", [userId]); }
  async setUserStatus(userId: string, status: User["status"]) { await this.db.query("UPDATE users SET status=$2 WHERE id=$1", [userId, status]); }
  async getIdentity(provider: string, subject: string): Promise<ExternalIdentity | undefined> {
    const r = await this.db.query("SELECT * FROM external_identities WHERE provider=$1 AND subject=$2", [provider, subject]);
    return r.rows[0] ? { provider: r.rows[0].provider, subject: r.rows[0].subject, userId: r.rows[0].user_id } : undefined;
  }
  async linkIdentity(i: ExternalIdentity) {
    await this.db.query("INSERT INTO external_identities(provider,subject,user_id) VALUES ($1,$2,$3) ON CONFLICT (provider,subject) DO NOTHING", [i.provider, i.subject, i.userId]);
  }

  async createWorkspace(w: { slug: string; name: string; ownerId: string }) {
    try {
      return await this.tx(async (q) => {
        const r = await q.query("INSERT INTO workspaces(id,slug,name) VALUES ($1,$2,$3) RETURNING *", [randomUUID(), w.slug, w.name]);
        await q.query("INSERT INTO memberships(workspace_id,user_id,role) VALUES ($1,$2,'owner')", [r.rows[0].id, w.ownerId]);
        return ws(r.rows[0]);
      });
    } catch (e) { return PgAuthRepository.dup(e, `workspace ${w.slug} exists`); }
  }
  async getWorkspace(idOrSlug: string) { const r = await this.db.query("SELECT * FROM workspaces WHERE id=$1 OR slug=$1 ORDER BY (id=$1) DESC LIMIT 1", [idOrSlug]); return r.rows[0] ? ws(r.rows[0]) : undefined; }
  async updateWorkspace(id: string, p: { name?: string }) { const r = await this.db.query("UPDATE workspaces SET name=COALESCE($2,name) WHERE id=$1 RETURNING *", [id, p.name ?? null]); return r.rows[0] ? ws(r.rows[0]) : undefined; }
  async deleteWorkspace(id: string) { const r = await this.db.query("DELETE FROM workspaces WHERE id=$1", [id]); return (r.rowCount ?? 0) > 0; }
  async listMembershipsForUser(userId: string) {
    const r = await this.db.query("SELECT m.*, w.id AS w_id, w.slug, w.name, w.created_at AS w_created FROM memberships m JOIN workspaces w ON w.id=m.workspace_id WHERE m.user_id=$1 ORDER BY w.slug", [userId]);
    return r.rows.map((x) => ({ ...mem(x), workspace: ws({ id: x.w_id, slug: x.slug, name: x.name, created_at: x.w_created }) }));
  }
  async getMembership(workspaceId: string, userId: string) { const r = await this.db.query("SELECT * FROM memberships WHERE workspace_id=$1 AND user_id=$2", [workspaceId, userId]); return r.rows[0] ? mem(r.rows[0]) : undefined; }
  async listMembers(workspaceId: string) {
    const r = await this.db.query("SELECT m.*, u.email, u.display_name FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.workspace_id=$1 ORDER BY u.email", [workspaceId]);
    return r.rows.map((x) => ({ ...mem(x), email: x.email as string, displayName: x.display_name as string }));
  }
  /** Serialises owner changes per workspace by locking the owner rows. */
  async setMember(workspaceId: string, userId: string, role: Role): Promise<MemberChange> {
    return this.tx(async (q) => {
      const owners = (await q.query("SELECT user_id FROM memberships WHERE workspace_id=$1 AND role='owner' FOR UPDATE", [workspaceId])).rows.map((x) => x.user_id);
      if (owners.length === 1 && owners[0] === userId && role !== "owner") return "last_owner";
      await q.query("INSERT INTO memberships(workspace_id,user_id,role) VALUES ($1,$2,$3) ON CONFLICT (workspace_id,user_id) DO UPDATE SET role=EXCLUDED.role", [workspaceId, userId, role]);
      return "ok";
    });
  }
  async removeMember(workspaceId: string, userId: string): Promise<MemberChange> {
    return this.tx(async (q) => {
      const owners = (await q.query("SELECT user_id FROM memberships WHERE workspace_id=$1 AND role='owner' FOR UPDATE", [workspaceId])).rows.map((x) => x.user_id);
      if (owners.length === 1 && owners[0] === userId) return "last_owner";
      const r = await q.query("DELETE FROM memberships WHERE workspace_id=$1 AND user_id=$2", [workspaceId, userId]);
      if (!r.rowCount) return "not_found";
      await q.query("UPDATE api_tokens SET revoked_at=now() WHERE workspace_id=$1 AND created_by=$2 AND revoked_at IS NULL", [workspaceId, userId]);
      return "ok";
    });
  }

  async createToken(t: Omit<ApiTokenRecord, "id" | "createdAt" | "lastUsedAt" | "revokedAt">) {
    try {
      const r = await this.db.query("INSERT INTO api_tokens(id,workspace_id,created_by,name,prefix,hash,scopes,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING *",
        [randomUUID(), t.workspaceId, t.createdBy, t.name, t.prefix, t.hash, t.scopes, t.expiresAt]);
      return tok(r.rows[0]);
    } catch (e) { return PgAuthRepository.dup(e, "token prefix collision"); }
  }
  async getTokenByPrefix(prefix: string) { const r = await this.db.query("SELECT * FROM api_tokens WHERE prefix=$1", [prefix]); return r.rows[0] ? tok(r.rows[0]) : undefined; }
  async getToken(id: string) { const r = await this.db.query("SELECT * FROM api_tokens WHERE id=$1", [id]); return r.rows[0] ? tok(r.rows[0]) : undefined; }
  async listTokens(workspaceId: string, createdBy?: string) {
    const r = await this.db.query("SELECT * FROM api_tokens WHERE workspace_id=$1 AND ($2::text IS NULL OR created_by=$2) ORDER BY created_at, id", [workspaceId, createdBy ?? null]);
    return r.rows.map(tok);
  }
  async revokeToken(id: string, now: Date) { const r = await this.db.query("UPDATE api_tokens SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL", [id, now.toISOString()]); return (r.rowCount ?? 0) > 0; }
  async touchToken(id: string, now: Date) { await this.db.query("UPDATE api_tokens SET last_used_at=$2 WHERE id=$1", [id, now.toISOString()]); }

  async createSession(s: { userId: string; expiresAt: Date; ip: string | null; userAgent: string | null }) {
    const r = await this.db.query("INSERT INTO sessions(id,user_id,expires_at,ip,user_agent) VALUES ($1,$2,$3,$4,$5) RETURNING *", [randomUUID(), s.userId, s.expiresAt.toISOString(), s.ip, s.userAgent]);
    return ses(r.rows[0]);
  }
  async getSession(id: string) { const r = await this.db.query("SELECT * FROM sessions WHERE id=$1", [id]); return r.rows[0] ? ses(r.rows[0]) : undefined; }
  async revokeSession(id: string, now: Date) { await this.db.query("UPDATE sessions SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL", [id, now.toISOString()]); }
  async revokeSessionsForUser(userId: string, now: Date) { await this.db.query("UPDATE sessions SET revoked_at=$2 WHERE user_id=$1 AND revoked_at IS NULL", [userId, now.toISOString()]); }

  async appendAudit(e: NewAuditEntry) {
    const r = await this.db.query(
      `INSERT INTO audit_log(id,ts,workspace_id,actor_type,actor_id,action,resource,resource_id,before_hash,after_hash,ip,request_id,status,metadata)
       VALUES ($1,COALESCE($2::timestamptz, now()),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING *`,
      [randomUUID(), e.ts ?? null, e.workspaceId, e.actorType, e.actorId, e.action, e.resource, e.resourceId, e.beforeHash, e.afterHash, e.ip, e.requestId, e.status, JSON.stringify(e.metadata)]);
    return aud(r.rows[0]);
  }
  async queryAudit(f: AuditFilter): Promise<AuditPage> {
    const r = await this.db.query(
      `SELECT * FROM audit_log WHERE workspace_id=$1
         AND ($2::bigint IS NULL OR seq < $2) AND ($3::text IS NULL OR actor_id=$3) AND ($4::text IS NULL OR action=$4)
         AND ($5::text IS NULL OR resource=$5) AND ($6::text IS NULL OR resource_id=$6)
         AND ($7::timestamptz IS NULL OR ts >= $7) AND ($8::timestamptz IS NULL OR ts < $8) AND ($9::int IS NULL OR status=$9)
       ORDER BY seq DESC LIMIT $10`,
      [f.workspaceId, f.before ?? null, f.actorId ?? null, f.action ?? null, f.resource ?? null, f.resourceId ?? null, f.from ?? null, f.to ?? null, f.status ?? null, f.limit + 1]);
    const rows = r.rows.map(aud);
    const items = rows.slice(0, f.limit);
    return { items, nextCursor: rows.length > f.limit ? String(items[items.length - 1]!.seq) : null };
  }
}
