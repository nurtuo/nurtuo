// SPDX-License-Identifier: AGPL-3.0-only
import { randomBytes, randomUUID } from "node:crypto";
import { ApiError } from "../domain.js";
import { DEFAULT_SCRYPT, PASSWORD_MAX, PASSWORD_MIN, TOKEN_PREFIX, generateApiToken, hashPassword, parseApiToken, safeEqual, sha256hex, signJwt, verifyJwt, verifyPassword, type ScryptParams } from "./crypto.js";
import type { Role } from "./permissions.js";
import { isPermission, type Permission } from "./permissions.js";
import type { AuthRepository } from "./repository.js";
import { FailureTracker, RateLimiter } from "./throttle.js";
import type { ApiTokenRecord, Principal, Session, User } from "./types.js";

export class RateLimited extends ApiError {
  constructor(public retryAfterSec: number, code = "rate_limited", message = "too many attempts, try again later") { super(429, code, message); }
}

export interface AuthServiceOptions {
  /** HS256 signing secret, at least 32 bytes. */
  secret: Buffer;
  issuer?: string; audience?: string;
  sessionTtlSec?: number;
  scrypt?: ScryptParams;
  maxFailures?: number; lockMs?: number;
  loginsPerMinutePerIp?: number;
  now?: () => Date;
}

export const SESSION_COOKIE = "__Host-nurtuo_session";
export const TOKEN_MAX_TTL_SEC = 365 * 86400;
export const TOKEN_DEFAULT_TTL_SEC = 90 * 86400;

export function parseCookies(h: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of (h ?? "").split(";")) {
    const i = part.indexOf("=");
    if (i > 0) { const k = part.slice(0, i).trim(); if (!(k in out)) out[k] = part.slice(i + 1).trim(); }
  }
  return out;
}

export class AuthService {
  readonly issuer: string; readonly audience: string; readonly sessionTtlSec: number;
  readonly scrypt: ScryptParams;
  private maxFailures: number; private lockMs: number;
  private ipLimiter: RateLimiter; private unknownFailures: FailureTracker;
  private dummyHash: Promise<string>;
  now: () => Date;

  constructor(readonly repo: AuthRepository, private o: AuthServiceOptions) {
    if (o.secret.length < 32) throw new Error("auth secret must be at least 32 bytes");
    this.now = o.now ?? (() => new Date());
    this.issuer = o.issuer ?? "nurtuo-control-plane";
    this.audience = o.audience ?? "nurtuo-api";
    this.sessionTtlSec = o.sessionTtlSec ?? 8 * 3600;
    this.scrypt = o.scrypt ?? DEFAULT_SCRYPT;
    this.maxFailures = o.maxFailures ?? 5;
    this.lockMs = o.lockMs ?? 15 * 60_000;
    this.ipLimiter = new RateLimiter(o.loginsPerMinutePerIp ?? 30, 60_000, () => this.now().getTime());
    this.unknownFailures = new FailureTracker(this.maxFailures, this.lockMs, () => this.now().getTime());
    this.dummyHash = hashPassword(randomBytes(16).toString("hex"), this.scrypt);
  }

  // ---- users -----------------------------------------------------------------------------------
  validateEmail(e: unknown): string {
    if (typeof e !== "string" || e.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(e)) throw new ApiError(400, "bad_request", "email is invalid");
    return e.trim().toLowerCase();
  }
  validatePassword(p: unknown): string {
    if (typeof p !== "string" || p.length < PASSWORD_MIN) throw new ApiError(400, "bad_request", `password must be at least ${PASSWORD_MIN} characters`);
    if (p.length > PASSWORD_MAX) throw new ApiError(400, "bad_request", `password must be at most ${PASSWORD_MAX} characters`);
    return p;
  }
  async createPasswordUser(email: string, password: string, displayName = ""): Promise<User> {
    return this.repo.createUser({ email: this.validateEmail(email), displayName, passwordHash: await hashPassword(this.validatePassword(password), this.scrypt) });
  }

  /** First-run helper: creates a user and an owned workspace when no one can log in yet. */
  async bootstrap(email: string, password: string, workspaceSlug = "default"): Promise<{ user: User; workspaceId: string }> {
    const user = (await this.repo.getUserByEmail(email)) ?? (await this.createPasswordUser(email, password, "Administrator"));
    const w = (await this.repo.getWorkspace(workspaceSlug)) ?? (await this.repo.createWorkspace({ slug: workspaceSlug, name: workspaceSlug, ownerId: user.id }));
    return { user, workspaceId: w.id };
  }

  /** Per-IP throttle for credential endpoints. Returns 0 when allowed, else seconds to wait. */
  limitIp(ip: string | null): number { return this.ipLimiter.hit(ip ?? "unknown"); }

  // ---- login / sessions --------------------------------------------------------------------------
  async login(email: unknown, password: unknown, ip: string | null, userAgent: string | null) {
    const wait = this.limitIp(ip);
    if (wait) throw new RateLimited(wait);
    const normalized = typeof email === "string" ? email.trim().toLowerCase().slice(0, 254) : "";
    const user = normalized ? await this.repo.getUserByEmail(normalized) : undefined;
    const now = this.now();
    const lockedSec = user
      ? (user.lockedUntil && new Date(user.lockedUntil) > now ? Math.ceil((new Date(user.lockedUntil).getTime() - now.getTime()) / 1000) : 0)
      : this.unknownFailures.lockedFor(normalized);
    if (lockedSec) throw new RateLimited(lockedSec, "account_locked", "too many failed attempts, try again later");

    const pw = typeof password === "string" && password.length <= PASSWORD_MAX ? password : "";
    // Always run one scrypt verification so timing does not reveal whether the account exists.
    const ok = user?.passwordHash ? await verifyPassword(pw, user.passwordHash) : (await verifyPassword(pw, await this.dummyHash), false);
    if (!user || !ok || user.status !== "active" || pw === "") {
      if (user) await this.repo.recordLoginFailure(user.id, this.maxFailures, this.lockMs, now); else this.unknownFailures.fail(normalized);
      throw new ApiError(401, "invalid_credentials", "invalid email or password");
    }
    await this.repo.recordLoginSuccess(user.id);
    this.unknownFailures.clear(normalized);
    return { user, ...(await this.startSession(user, ip, userAgent)) };
  }

  async startSession(user: User, ip: string | null, userAgent: string | null) {
    const exp = new Date(this.now().getTime() + this.sessionTtlSec * 1000);
    const session = await this.repo.createSession({ userId: user.id, expiresAt: exp, ip, userAgent: userAgent?.slice(0, 256) ?? null });
    const csrf = randomBytes(24).toString("base64url");
    const iat = Math.floor(this.now().getTime() / 1000);
    const jwt = signJwt({ iss: this.issuer, aud: this.audience, sub: user.id, sid: session.id, csrf, iat, exp: Math.floor(exp.getTime() / 1000) }, this.o.secret);
    return { session, jwt, csrf, expiresAt: exp.toISOString() };
  }

  sessionCookie(jwt: string): string { return `${SESSION_COOKIE}=${jwt}; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=${this.sessionTtlSec}`; }
  clearCookie(): string { return `${SESSION_COOKIE}=; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=0`; }
  signState(claims: Record<string, unknown>, ttlSec: number): string {
    const iat = Math.floor(this.now().getTime() / 1000);
    return signJwt({ iss: this.issuer, aud: "nurtuo-oidc-state", iat, exp: iat + ttlSec, ...claims }, this.o.secret);
  }
  verifyState(t: string): Record<string, any> | null {
    return verifyJwt(t, this.o.secret, { iss: this.issuer, aud: "nurtuo-oidc-state", now: Math.floor(this.now().getTime() / 1000) });
  }

  // ---- authentication ------------------------------------------------------------------------------
  /** null: no credentials presented. Throws 401 when credentials are presented but invalid. */
  async authenticate(headers: Record<string, string | undefined>): Promise<Principal | null> {
    const authz = headers["authorization"];
    if (authz !== undefined) {
      const m = /^Bearer ([^\s]+)$/i.exec(authz);
      if (!m) throw unauthenticated();
      return m[1]!.startsWith(TOKEN_PREFIX) ? this.authenticateToken(m[1]!) : this.authenticateJwt(m[1]!, false);
    }
    const cookie = parseCookies(headers["cookie"])[SESSION_COOKIE];
    return cookie ? this.authenticateJwt(cookie, true) : null;
  }

  private async memberships(userId: string) {
    return new Map<string, Role>((await this.repo.listMembershipsForUser(userId)).map((m) => [m.workspaceId, m.role]));
  }

  private async authenticateJwt(jwt: string, viaCookie: boolean): Promise<Principal> {
    const c = verifyJwt(jwt, this.o.secret, { iss: this.issuer, aud: this.audience, now: Math.floor(this.now().getTime() / 1000) });
    if (!c || typeof c.sid !== "string" || typeof c.sub !== "string") throw unauthenticated();
    const s: Session | undefined = await this.repo.getSession(c.sid);
    if (!s || s.revokedAt || s.userId !== c.sub || new Date(s.expiresAt) <= this.now()) throw unauthenticated();
    const user = await this.repo.getUserById(s.userId);
    if (!user || user.status !== "active") throw unauthenticated();
    return { kind: "user", userId: user.id, tokenWorkspaceId: null, sessionId: s.id, csrf: String(c.csrf ?? ""), tokenScopes: null, memberships: await this.memberships(user.id), viaCookie };
  }

  private async authenticateToken(raw: string): Promise<Principal> {
    const parsed = parseApiToken(raw);
    if (!parsed) throw unauthenticated();
    const rec = await this.repo.getTokenByPrefix(parsed.prefix);
    // Compare against a fixed-size dummy when the prefix is unknown: one code path, constant-time compare.
    const hashOk = safeEqual(sha256hex(parsed.secret), rec?.hash ?? sha256hex("x".repeat(43)));
    const now = this.now();
    if (!rec || !hashOk || rec.revokedAt || (rec.expiresAt && new Date(rec.expiresAt) <= now)) throw unauthenticated();
    const user = await this.repo.getUserById(rec.createdBy);
    const m = user?.status === "active" ? await this.repo.getMembership(rec.workspaceId, rec.createdBy) : undefined;
    if (!m) throw unauthenticated(); // creator disabled or removed from the workspace: token is dead
    if (!rec.lastUsedAt || now.getTime() - new Date(rec.lastUsedAt).getTime() > 60_000) await this.repo.touchToken(rec.id, now);
    return {
      kind: "token", userId: rec.createdBy, tokenWorkspaceId: rec.workspaceId, tokenId: rec.id,
      tokenScopes: new Set(rec.scopes), tokenExpiresAt: rec.expiresAt, memberships: new Map([[rec.workspaceId, m.role]]), viaCookie: false,
    };
  }

  // ---- API tokens ----------------------------------------------------------------------------------
  /**
   * Mints a token. Scopes must be a subset of the caller's own effective permissions in the workspace
   * (no scope widening), and a token-created token can never outlive its parent.
   */
  async mintToken(p: { workspaceId: string; creator: Principal; effective: ReadonlySet<Permission>; name: unknown; scopes: unknown; expiresInSec: unknown }): Promise<{ record: ApiTokenRecord; token: string }> {
    if (typeof p.name !== "string" || !p.name.trim() || p.name.length > 100) throw new ApiError(400, "bad_request", "name must be a non-empty string up to 100 chars");
    if (!Array.isArray(p.scopes) || p.scopes.length === 0 || p.scopes.length > 64) throw new ApiError(400, "bad_request", "scopes must be a non-empty array");
    const scopes: Permission[] = [];
    for (const s of p.scopes) {
      if (!isPermission(s)) throw new ApiError(400, "bad_request", `unknown scope ${JSON.stringify(s)}`);
      if (!p.effective.has(s)) throw new ApiError(403, "scope_exceeds_permissions", `cannot grant scope ${s}: you do not hold it`);
      if (!scopes.includes(s)) scopes.push(s);
    }
    let ttl = TOKEN_DEFAULT_TTL_SEC;
    if (p.expiresInSec !== undefined) {
      if (typeof p.expiresInSec !== "number" || !Number.isInteger(p.expiresInSec) || p.expiresInSec < 60 || p.expiresInSec > TOKEN_MAX_TTL_SEC) throw new ApiError(400, "bad_request", `expiresInSeconds must be an integer between 60 and ${TOKEN_MAX_TTL_SEC}`);
      ttl = p.expiresInSec;
    }
    let expires = this.now().getTime() + ttl * 1000;
    if (p.creator.tokenExpiresAt) expires = Math.min(expires, new Date(p.creator.tokenExpiresAt).getTime());
    const g = generateApiToken();
    const record = await this.repo.createToken({ workspaceId: p.workspaceId, createdBy: p.creator.userId, name: p.name.trim(), prefix: g.prefix, hash: g.hash, scopes, expiresAt: new Date(expires).toISOString() });
    return { record, token: g.token };
  }
}

const unauthenticated = () => new ApiError(401, "unauthenticated", "invalid or missing credentials");
export const newRequestId = () => randomUUID();
