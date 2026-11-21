// SPDX-License-Identifier: AGPL-3.0-only
// Crypto primitives on node:crypto only: scrypt passwords, HS256 JWT, API token format, timing-safe helpers.
import { createHash, createHmac, randomBytes, scrypt as scryptCb, timingSafeEqual, type BinaryLike } from "node:crypto";

export const b64u = (b: Buffer | string) => Buffer.from(b).toString("base64url");
export const sha256hex = (s: string) => createHash("sha256").update(s).digest("hex");

/** Constant-time string equality that also hides length differences (compares fixed-size digests). */
export function safeEqual(a: string, b: string): boolean {
  const ha = createHash("sha256").update(a).digest();
  const hb = createHash("sha256").update(b).digest();
  return timingSafeEqual(ha, hb) && a.length === b.length;
}

// ---- passwords (scrypt) ----------------------------------------------------------------------
export interface ScryptParams { N: number; r: number; p: number }
export const DEFAULT_SCRYPT: ScryptParams = { N: 16384, r: 8, p: 1 };
const MAX_N = 1 << 17; // refuse absurd stored parameters (DoS via tampered rows)
export const PASSWORD_MIN = 12;
export const PASSWORD_MAX = 256;

function scrypt(pw: BinaryLike, salt: Buffer, keylen: number, o: ScryptParams): Promise<Buffer> {
  return new Promise((res, rej) => scryptCb(pw, salt, keylen, { N: o.N, r: o.r, p: o.p, maxmem: 256 * o.N * o.r + (1 << 20) }, (e, k) => (e ? rej(e) : res(k))));
}

export async function hashPassword(password: string, params: ScryptParams = DEFAULT_SCRYPT): Promise<string> {
  const salt = randomBytes(16);
  const key = await scrypt(password.normalize("NFKC"), salt, 32, params);
  return `scrypt$${params.N}$${params.r}$${params.p}$${b64u(salt)}$${b64u(key)}`;
}

export async function verifyPassword(password: string, stored: string): Promise<boolean> {
  const parts = stored.split("$");
  if (parts.length !== 6 || parts[0] !== "scrypt") return false;
  const [N, r, p] = [Number(parts[1]), Number(parts[2]), Number(parts[3])];
  if (![N, r, p].every(Number.isInteger) || N < 2 || N > MAX_N || (N & (N - 1)) !== 0 || r < 1 || r > 32 || p < 1 || p > 16) return false;
  const salt = Buffer.from(parts[4]!, "base64url");
  const want = Buffer.from(parts[5]!, "base64url");
  if (want.length < 16) return false;
  const got = await scrypt(password.normalize("NFKC"), salt, want.length, { N, r, p });
  return timingSafeEqual(got, want);
}

// ---- JWT HS256 -------------------------------------------------------------------------------
export interface JwtClaims { iss: string; aud: string; sub: string; sid: string; csrf: string; iat: number; exp: number; [k: string]: unknown }
const HEADER = b64u(JSON.stringify({ alg: "HS256", typ: "JWT" }));

export function signJwt(claims: Record<string, unknown>, secret: Buffer): string {
  const body = `${HEADER}.${b64u(JSON.stringify(claims))}`;
  return `${body}.${b64u(createHmac("sha256", secret).update(body).digest())}`;
}

/** Returns claims or null. Header alg is pinned to HS256 (no "none", no algorithm confusion). */
export function verifyJwt(token: string, secret: Buffer, opts: { iss: string; aud: string; now: number }): Record<string, any> | null {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  const [h, p, s] = parts as [string, string, string];
  try {
    const header = JSON.parse(Buffer.from(h, "base64url").toString("utf8"));
    if (header?.alg !== "HS256" || header?.typ !== "JWT") return null;
    const want = createHmac("sha256", secret).update(`${h}.${p}`).digest();
    const got = Buffer.from(s, "base64url");
    if (got.length !== want.length || !timingSafeEqual(got, want)) return null;
    const c = JSON.parse(Buffer.from(p, "base64url").toString("utf8"));
    if (!c || typeof c !== "object") return null;
    if (c.iss !== opts.iss || c.aud !== opts.aud) return null;
    if (typeof c.exp !== "number" || c.exp <= opts.now) return null;
    if (typeof c.nbf === "number" && c.nbf > opts.now + 5) return null;
    return c;
  } catch { return null; }
}

// ---- API tokens: nrt_<prefix>_<secret> -------------------------------------------------------
export const TOKEN_PREFIX = "nrt_";
export interface GeneratedToken { token: string; prefix: string; hash: string }
export function generateApiToken(): GeneratedToken {
  const prefix = randomBytes(6).toString("hex"); // 12 hex chars, public lookup key
  const secret = randomBytes(32).toString("base64url");
  return { token: `${TOKEN_PREFIX}${prefix}_${secret}`, prefix, hash: sha256hex(secret) };
}
export function parseApiToken(t: string): { prefix: string; secret: string } | null {
  const m = /^nrt_([0-9a-f]{12})_([A-Za-z0-9_-]{43})$/.exec(t);
  return m ? { prefix: m[1]!, secret: m[2]! } : null;
}
