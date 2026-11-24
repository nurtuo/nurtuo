// SPDX-License-Identifier: AGPL-3.0-only
// Connection validation at save time, mirroring the Go rules (connections.Parse / CheckLiterals and
// the SFTP connector) so the user gets the error immediately; the `nurtuo flow run` child validates again.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiError } from "../domain.js";
import { SECRET_REF } from "./secrets.js";

export const CONNECTION_ID = /^[a-z][a-z0-9_-]{0,63}$/;
/** Connectors a workspace connection may use in this slice (canvas-run D11). */
export const TEST_RUN_CONNECTORS = ["sftp"] as const;

interface Schema { properties: Record<string, { writeOnly?: boolean; "x-nurtuo-secret"?: boolean; type?: string }> }
let sftpSchema: Schema | undefined;
/** The copy of connectors/sftp/schemas/connection.json that the build puts into dist. */
export function sftpConnectionSchema(): Schema {
  sftpSchema ??= JSON.parse(readFileSync(fileURLToPath(new URL("./schemas/sftp.connection.json", import.meta.url)), "utf8")) as Schema;
  return sftpSchema;
}

/** Fields that hold a secret (`writeOnly` or `x-nurtuo-secret` in the connector's schema). */
export function secretFields(connector: string): string[] {
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
  if (!(TEST_RUN_CONNECTORS as readonly string[]).includes(connector)) throw bad("not_allowed_in_test_run", `connector "${connector}" is not allowed in test runs (only ${TEST_RUN_CONNECTORS.join(", ")})`);
  if (!settings || typeof settings !== "object" || Array.isArray(settings)) throw bad("bad_request", "settings must be a JSON object");
  const s = { ...(settings as Record<string, unknown>) };
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

/** Secret names referenced by settings, per field. */
export function secretRefs(connector: string, settings: Record<string, unknown>): Array<{ field: string; name: string }> {
  const out: Array<{ field: string; name: string }> = [];
  for (const f of secretFields(connector)) {
    const m = typeof settings[f] === "string" ? SECRET_REF.exec(settings[f] as string) : null;
    if (m) out.push({ field: f, name: m[1]! });
  }
  return out;
}
