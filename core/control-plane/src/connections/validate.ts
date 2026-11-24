// SPDX-License-Identifier: AGPL-3.0-only
// Connection validation at save time, mirroring the Go rules (connections.Parse / CheckLiterals and
// the SFTP connector) so the user gets the error immediately; the `nurtuo flow run` child validates again.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiError } from "../domain.js";
import { SECRET_REF } from "./secrets.js";

export const CONNECTION_ID = /^[a-z][a-z0-9_-]{0,63}$/;
/** Connectors a workspace connection may use in this slice (canvas-run D11). */
export const WORKSPACE_CONNECTORS = ["sftp", "http", "postgres"] as const;

interface Schema { properties: Record<string, { writeOnly?: boolean; "x-nurtuo-secret"?: boolean; type?: string }> }
let sftpSchema: Schema | undefined;
/** The copy of connectors/sftp/schemas/connection.json that the build puts into dist. */
export function sftpConnectionSchema(): Schema {
  sftpSchema ??= JSON.parse(readFileSync(fileURLToPath(new URL("./schemas/sftp.connection.json", import.meta.url)), "utf8")) as Schema;
  return sftpSchema;
}

/** Fields that hold a secret (`writeOnly` or `x-nurtuo-secret` in the connector's schema). */
export function secretFields(connector: string): string[] {
  if (connector === "http") return ["password", "token", "apiKeyValue"];
  if (connector === "postgres") return ["password"];
  if (connector !== "sftp") return [];
  return Object.entries(sftpConnectionSchema().properties).filter(([, p]) => p.writeOnly || p["x-nurtuo-secret"]).map(([k]) => k);
}

const bad = (code: string, message: string) => new ApiError(400, code, message);

export function validateConnectionId(id: string): void {
  if (!CONNECTION_ID.test(id)) throw bad("bad_request", "connection id must match ^[a-z][a-z0-9_-]{0,63}$");
}

/** Returns the settings to store (a copy), or throws a 400 ApiError with the spec's code. */
export function validateConnection(connector: unknown, settings: unknown): Record<string, unknown> {
  if (typeof connector !== "string" || connector === "") throw bad("bad_request", "connector must be a non-empty string");
  if (!(WORKSPACE_CONNECTORS as readonly string[]).includes(connector)) throw bad("not_allowed_in_test_run", `connector "${connector}" is not allowed in test runs (only ${WORKSPACE_CONNECTORS.join(", ")})`);
  if (!settings || typeof settings !== "object" || Array.isArray(settings)) throw bad("bad_request", "settings must be a JSON object");
  const s = { ...(settings as Record<string, unknown>) };
  if (connector === "http" || connector === "postgres") {
    validateNetworkSettings(connector, s);
    return s;
  }
  const props = sftpConnectionSchema().properties;
  for (const k of Object.keys(s)) if (!Object.hasOwn(props, k)) throw bad("bad_request", `unknown setting "${k}"`);
  const str = (k: string) => { const v = s[k]; if (v !== undefined && (typeof v !== "string")) throw bad("bad_request", `${k} must be a string`); return v as string | undefined; };
  const host = str("host");
  if (!host || !/^[a-z0-9_:.%\[\]-]{1,253}$/i.test(host) || host.startsWith("-")) throw bad("bad_request", "enter a valid SSH host");
  if (!str("user") || Buffer.byteLength(s.user as string) > 255) throw bad("bad_request", "user is required and must be at most 255 bytes");
  if (s.port !== undefined && (!Number.isInteger(s.port) || (s.port as number) < 1 || (s.port as number) > 65535)) throw bad("bad_request", "port must be an integer between 1 and 65535");
  for (const k of ["maxListEntries", "timeoutMs"]) if (s[k] !== undefined && (!Number.isInteger(s[k]) || (s[k] as number) < 1)) throw bad("bad_request", `${k} must be a positive integer`);
  for (const k of ["rootPath", "knownHosts", "hostKeyFingerprint", "hostKeyPolicy"]) str(k);
  if (Buffer.byteLength((s.rootPath as string | undefined) ?? "") > 4096) throw bad("bad_request", "rootPath must be at most 4 KiB");
  for (const k of secretFields(connector)) {
    const v = s[k];
    if (v === undefined) continue;
    if (typeof v !== "string" || !SECRET_REF.test(v)) throw bad("literal_secret", `${k} must be a secret reference (secret://<name>), never a literal value`);
  }
  if (s.password === undefined && s.privateKey === undefined) throw bad("bad_request", "password or privateKey is required");
  const policy = (s.hostKeyPolicy as string | undefined) ?? "known_hosts";
  if (policy === "insecure") throw bad("insecure_host_key_not_allowed", "hostKeyPolicy insecure is not allowed; trust the server's host key fingerprint instead");
  if (policy === "known_hosts") { if (!(s.knownHosts as string | undefined)?.trim()) throw bad("host_key_required", "knownHosts content is required for hostKeyPolicy known_hosts"); }
  else if (policy === "fingerprint") { if (!/^SHA256:[A-Za-z0-9+/]{43}$/.test(String(s.hostKeyFingerprint ?? ""))) throw bad("host_key_required", "hostKeyFingerprint is required for hostKeyPolicy fingerprint"); }
  else throw bad("bad_request", "hostKeyPolicy must be known_hosts or fingerprint");
  return s;
}

function validateNetworkSettings(connector: string, s: Record<string, unknown>) {
  const allowed = connector === "http" ? ["baseUrl", "auth"] : ["host", "port", "database", "user", "sslMode", "password"];
  if (Object.keys(s).some((key) => !allowed.includes(key))) throw bad("bad_request", "unsupported connection setting");
  if (connector === "postgres") {
    if (typeof s.host !== "string" || !/^[a-z0-9][a-z0-9.-]{0,252}$/i.test(s.host)) throw bad("bad_request", "enter a valid database hostname or IPv4 address");
    for (const key of ["user", "database"]) {
      const value = s[key];
      if (typeof value !== "string" || !value.trim() || value.includes("\0")) throw bad("bad_request", `${key} is required`);
    }
    if (s.port !== undefined && (!Number.isInteger(s.port) || (s.port as number) < 1 || (s.port as number) > 65535)) throw bad("bad_request", "port must be an integer between 1 and 65535");
    if (s.sslMode !== undefined && !["disable", "require", "verify-full"].includes(String(s.sslMode))) throw bad("bad_request", "unsupported sslMode");
  } else {
    const url = typeof s.baseUrl === "string" ? URL.parse(s.baseUrl) : null;
    if (!url || !["http:", "https:"].includes(url.protocol) || url.username || url.password || url.search || url.hash || !/^https?:\/\/[^/]+/i.test(String(s.baseUrl)) || /[\\\s]/.test(String(s.baseUrl))) {
      throw bad("bad_request", "enter an HTTP(S) base URL without user information, query or fragment");
    }
    const auth = s.auth === undefined ? {} : s.auth;
    if (!auth || typeof auth !== "object" || Array.isArray(auth)) throw bad("bad_request", "auth must be an object");
    const a = auth as Record<string, unknown>;
    if (Object.keys(a).some((key) => !["type", "username", "apiKeyName", ...secretFields(connector)].includes(key))) throw bad("bad_request", "unsupported HTTP auth setting");
    const type = a.type ?? "none";
    const mode = type === "basic" ? "password" : type === "bearer" ? "token" : type === "apikey" ? "apiKeyValue" : undefined;
    if (type !== "none" && !mode) throw bad("bad_request", "unsupported HTTP authentication mode");
    if (mode && (typeof a[mode] !== "string" || !a[mode])) throw bad("invalid_credential_set", "enter the credential for the selected authentication mode");
    if (type === "basic" && (typeof a.username !== "string" || !a.username || /[:\x00-\x1f\x7f]/.test(a.username))) throw bad("bad_request", "enter a valid HTTP username");
    if (type === "apikey" && (typeof a.apiKeyName !== "string" || !/^x-[a-z0-9-]+$/i.test(a.apiKeyName))) throw bad("bad_request", "enter an API key header name beginning with X-");
    for (const field of secretFields(connector)) {
      if (field !== mode && Object.hasOwn(a, field)) throw bad("invalid_credential_set", "remove credentials for the previous authentication mode");
    }
  }
  for (const field of secretFields(connector)) {
    const value = credentialSlots(connector, s)[field];
    if (value !== undefined && (typeof value !== "string" || !SECRET_REF.test(value))) throw bad("literal_secret", "credential must be a server-issued secret reference");
  }
}

export function credentialSlots(connector: string, settings: Record<string, unknown>): Record<string, unknown> {
  return connector === "http" ? (settings.auth ?? {}) as Record<string, unknown> : settings;
}

/** Secret names referenced by settings, per field. */
export function secretRefs(connector: string, settings: Record<string, unknown>): Array<{ field: string; name: string }> {
  const out: Array<{ field: string; name: string }> = [];
  const slots = credentialSlots(connector, settings);
  for (const f of secretFields(connector)) {
    const m = typeof slots[f] === "string" ? SECRET_REF.exec(slots[f] as string) : null;
    if (m) out.push({ field: f, name: m[1]! });
  }
  return out;
}
