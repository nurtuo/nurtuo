// SPDX-License-Identifier: AGPL-3.0-only
// OIDC authorization-code flow with PKCE (S256). Implemented on node:crypto + fetch; ID tokens are
// verified locally against the issuer's JWKS (RS256 only; "none"/HS* are rejected on purpose).
import { createHash, createPublicKey, randomBytes, verify as cryptoVerify } from "node:crypto";
import { ApiError } from "../domain.js";
import { safeEqual } from "./crypto.js";

/** Subset of fetch the flow needs; tests inject a fake IdP. */
export type OidcFetch = (url: string, init?: { method?: string; headers?: Record<string, string>; body?: string; signal?: AbortSignal }) => Promise<{ ok: boolean; status: number; json(): Promise<any> }>;

export interface OidcConfig {
  issuer: string; clientId: string; clientSecret?: string; redirectUri: string;
  scopes?: string[]; fetch?: OidcFetch; now?: () => number;
}
export interface OidcIdentity { issuer: string; subject: string; email?: string; emailVerified: boolean; name?: string }
export interface OidcBegin { authorizationUrl: string; state: string; nonce: string; codeVerifier: string }

/** The seam used by the HTTP layer; swap in another implementation (or a fake) without touching routes. */
export interface OidcProvider {
  begin(): Promise<OidcBegin>;
  complete(input: { code: string; state: string; expectedState: string; nonce: string; codeVerifier: string }): Promise<OidcIdentity>;
}

const rand = (n = 32) => randomBytes(n).toString("base64url");
const fail = (msg: string) => new ApiError(401, "oidc_failed", msg);

export class OidcClient implements OidcProvider {
  private doFetch: OidcFetch;
  private disco?: { authorization_endpoint: string; token_endpoint: string; jwks_uri: string; issuer: string };
  constructor(private c: OidcConfig) {
    this.doFetch = c.fetch ?? ((u, i) => fetch(u, i as RequestInit) as any);
    if (!c.issuer.startsWith("https://") && !/^http:\/\/(localhost|127\.0\.0\.1)/.test(c.issuer)) throw new Error("OIDC issuer must be https");
  }

  private async getJson(url: string, init?: Parameters<OidcFetch>[1]) {
    const r = await this.doFetch(url, { ...init, signal: AbortSignal.timeout(5000) });
    if (!r.ok) throw fail(`idp request failed (${r.status})`);
    return r.json();
  }
  private async discovery() {
    if (this.disco) return this.disco;
    const d = await this.getJson(`${this.c.issuer.replace(/\/$/, "")}/.well-known/openid-configuration`);
    if (d?.issuer !== this.c.issuer || !d.authorization_endpoint || !d.token_endpoint || !d.jwks_uri) throw fail("invalid discovery document");
    return (this.disco = d);
  }

  async begin(): Promise<OidcBegin> {
    const d = await this.discovery();
    const state = rand(), nonce = rand(), codeVerifier = rand(48);
    const challenge = createHash("sha256").update(codeVerifier).digest("base64url");
    const q = new URLSearchParams({
      response_type: "code", client_id: this.c.clientId, redirect_uri: this.c.redirectUri,
      scope: (this.c.scopes ?? ["openid", "email", "profile"]).join(" "), state, nonce,
      code_challenge: challenge, code_challenge_method: "S256",
    });
    return { authorizationUrl: `${d.authorization_endpoint}${d.authorization_endpoint.includes("?") ? "&" : "?"}${q}`, state, nonce, codeVerifier };
  }

  async complete(i: { code: string; state: string; expectedState: string; nonce: string; codeVerifier: string }): Promise<OidcIdentity> {
    if (!safeEqual(i.state, i.expectedState)) throw fail("state mismatch");
    const d = await this.discovery();
    const body = new URLSearchParams({ grant_type: "authorization_code", code: i.code, redirect_uri: this.c.redirectUri, client_id: this.c.clientId, code_verifier: i.codeVerifier });
    if (this.c.clientSecret) body.set("client_secret", this.c.clientSecret);
    const tok = await this.getJson(d.token_endpoint, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded", accept: "application/json" }, body: body.toString() });
    if (typeof tok?.id_token !== "string") throw fail("no id_token in token response");
    const claims = await this.verifyIdToken(tok.id_token, d.jwks_uri);
    if (typeof claims.nonce !== "string" || !safeEqual(claims.nonce, i.nonce)) throw fail("nonce mismatch");
    return { issuer: claims.iss, subject: claims.sub, email: typeof claims.email === "string" ? claims.email.toLowerCase() : undefined, emailVerified: claims.email_verified === true, name: typeof claims.name === "string" ? claims.name : undefined };
  }

  private async verifyIdToken(jwt: string, jwksUri: string): Promise<Record<string, any>> {
    const parts = jwt.split(".");
    if (parts.length !== 3) throw fail("malformed id_token");
    const [h, p, s] = parts as [string, string, string];
    let header: any, claims: any;
    try { header = JSON.parse(Buffer.from(h, "base64url").toString()); claims = JSON.parse(Buffer.from(p, "base64url").toString()); } catch { throw fail("malformed id_token"); }
    if (header?.alg !== "RS256") throw fail("unsupported id_token alg");
    const jwks = await this.getJson(jwksUri);
    const keys: any[] = (jwks?.keys ?? []).filter((k: any) => k.kty === "RSA" && (!k.use || k.use === "sig"));
    const key = header.kid ? keys.find((k) => k.kid === header.kid) : keys.length === 1 ? keys[0] : undefined;
    if (!key) throw fail("signing key not found");
    const ok = cryptoVerify("RSA-SHA256", Buffer.from(`${h}.${p}`), createPublicKey({ key, format: "jwk" }), Buffer.from(s, "base64url"));
    if (!ok) throw fail("bad id_token signature");
    const now = Math.floor((this.c.now?.() ?? Date.now()) / 1000);
    const aud: string[] = Array.isArray(claims.aud) ? claims.aud : [claims.aud];
    if (claims.iss !== this.c.issuer) throw fail("issuer mismatch");
    if (!aud.includes(this.c.clientId)) throw fail("audience mismatch");
    if (aud.length > 1 && claims.azp !== this.c.clientId) throw fail("azp mismatch");
    if (typeof claims.exp !== "number" || claims.exp + 60 <= now) throw fail("id_token expired");
    if (typeof claims.iat === "number" && claims.iat > now + 300) throw fail("id_token issued in the future");
    if (typeof claims.sub !== "string" || !claims.sub) throw fail("missing sub");
    return claims;
  }
}
