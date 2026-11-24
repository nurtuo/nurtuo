// SPDX-License-Identifier: AGPL-3.0-only
import { createHash } from "node:crypto";
import { canonicalJson } from "../api.js";
import { ApiError } from "../domain.js";
import type { ConnectionWrite } from "./repository.js";
import { secretFields, secretRefs } from "./validate.js";

export function binding(c: ConnectionWrite, field: string): string {
  const s = c.settings;
  return createHash("sha256").update(canonicalJson({
    connector: c.connector, field, incarnation: c.incarnation,
    host: logicalHost(String(s.host)), port: s.port ?? 22, user: s.user,
    authMode: secretRefs(c.connector, s).map((r) => r.field).sort(),
    hostKeyPolicy: s.hostKeyPolicy, fingerprint: s.hostKeyFingerprint, knownHosts: s.knownHosts,
  })).digest("hex");
}

export function checkSecretPlacement(connector: string, settings: Record<string, unknown>, allowSlots: boolean) {
  const fields = secretFields(connector);
  for (const [key, value] of Object.entries(settings)) {
    if (typeof value === "string" && value.startsWith("secret://") && allowSlots && fields.includes(key)) continue;
    if (JSON.stringify(value)?.includes("secret://") || (fields.includes(key) && !allowSlots)) {
      throw new ApiError(400, "secret_ref_not_allowed", "credential references are assigned by the server");
    }
  }
}

/** DNS queries use this absolute spelling; IP addresses retain the case of their zone ID. */
export function logicalHost(host: string): string {
  const zone = host.indexOf("%");
  if (zone >= 0) return host.slice(0, zone).toLowerCase() + host.slice(zone);
  return host.toLowerCase().replace(/\.$/, "");
}
