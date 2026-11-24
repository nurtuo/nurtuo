// SPDX-License-Identifier: AGPL-3.0-only
import { createHash } from "node:crypto";
import { canonicalJson } from "../api.js";
import { ApiError } from "../domain.js";
import type { ConnectionWrite } from "./repository.js";
import { credentialSlots, secretFields, secretRefs } from "./validate.js";

export function binding(c: ConnectionWrite, field: string): string {
  const s = c.settings;
  const identity = { connector: c.connector, field, incarnation: c.incarnation };
  if (c.connector === "http") {
    const auth = credentialSlots(c.connector, s);
    return digest({ ...identity, baseUrl: s.baseUrl, type: auth.type, username: auth.username, apiKeyName: auth.apiKeyName });
  }
  if (c.connector === "postgres") {
    return digest({ ...identity, host: logicalHost(String(s.host)), port: s.port ?? 5432, database: s.database, user: s.user, sslMode: s.sslMode ?? "require" });
  }
  return digest({
    ...identity,
    host: logicalHost(String(s.host)), port: s.port ?? 22, user: s.user,
    authMode: secretRefs(c.connector, s).map((r) => r.field).sort(),
    hostKeyPolicy: s.hostKeyPolicy, fingerprint: s.hostKeyFingerprint, knownHosts: s.knownHosts,
  });
}

function digest(value: Record<string, unknown>): string {
  return createHash("sha256").update(canonicalJson(value)).digest("hex");
}

export function checkSecretPlacement(connector: string, settings: Record<string, unknown>, allowSlots: boolean) {
  const fields = secretFields(connector);
  const reject = () => new ApiError(400, "secret_ref_not_allowed", "credential references are assigned by the server");
  const check = (key: string, value: unknown, slot: boolean) => {
    if (typeof value === "string" && value.startsWith("secret://") && allowSlots && slot) return;
    if (key.includes("secret://") || JSON.stringify(value)?.includes("secret://") || slot) throw reject();
  };
  for (const [key, value] of Object.entries(settings)) {
    if (connector === "http" && key === "auth" && value && typeof value === "object" && !Array.isArray(value)) {
      for (const [name, entry] of Object.entries(value)) check(name, entry, fields.includes(name));
    } else {
      check(key, value, connector !== "http" && fields.includes(key));
    }
  }
}

/** DNS queries use this absolute spelling; IP addresses retain the case of their zone ID. */
export function logicalHost(host: string): string {
  const zone = host.indexOf("%");
  if (zone >= 0) return host.slice(0, zone).toLowerCase() + host.slice(zone);
  return host.toLowerCase().replace(/\.$/, "");
}
