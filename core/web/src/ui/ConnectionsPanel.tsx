// SPDX-License-Identifier: AGPL-3.0-only
import { useState } from "react";
import sftpSchema from "../../../../connectors/sftp/schemas/connection.json";
import { ApiError, type ConnectionItem, type ControlPlane } from "../api/controlPlane";
import { fieldsOf, type Field } from "../model/formSchema";

// Generated from the connector's own connection schema. This slice offers password authentication
// and a pinned host key only: no privateKey/passphrase, no `insecure`, no oneOf handling.
const HIDDEN = new Set(["privateKey", "passphrase", "hostKeyPolicy", "knownHosts", "hostKeyFingerprint", "maxListEntries", "timeoutMs"]);
const SFTP_FIELDS: Field[] = fieldsOf(sftpSchema as Record<string, unknown>).filter((f) => !HIDDEN.has(f.name));
const POSTGRES_FIELDS = fieldsOf({
  required: ["host", "database", "user"],
  properties: {
    host: { type: "string" },
    port: { type: "integer", minimum: 1, maximum: 65535 },
    database: { type: "string" },
    user: { type: "string" },
    sslMode: { enum: ["require", "verify-full", "disable"] },
    password: { type: "string", writeOnly: true },
  },
});
const HTTP_FIELDS = fieldsOf({
  required: ["baseUrl", "username", "apiKeyName"],
  properties: {
    baseUrl: { type: "string" },
    type: { enum: ["none", "basic", "bearer", "apikey"] },
    username: { type: "string" },
    password: { type: "string", writeOnly: true },
    token: { type: "string", writeOnly: true },
    apiKeyName: { type: "string" },
    apiKeyValue: { type: "string", writeOnly: true },
  },
});
type Connector = "sftp" | "http" | "postgres";
const DEFAULTS: Record<Connector, Record<string, string>> = {
  sftp: { port: "22" },
  http: { type: "none" },
  postgres: { port: "5432", sslMode: "require" },
};
function connectionFields(connector: Connector, mode?: string) {
  if (connector === "sftp") return SFTP_FIELDS;
  if (connector === "postgres") return POSTGRES_FIELDS;
  return HTTP_FIELDS.filter(({ name }) => ["baseUrl", "type"].includes(name)
    || (mode === "basic" && ["username", "password"].includes(name))
    || (mode === "bearer" && name === "token")
    || (mode === "apikey" && ["apiKeyName", "apiKeyValue"].includes(name)));
}
const ID_RE = /^[a-z][a-z0-9_-]*$/;

/** Workspace connections: list, create/edit a connection, delete. Credential values are write-only. */
export function ConnectionsPanel({ api, workspaceId, items, onChanged, onClose }: {
  api: ControlPlane;
  workspaceId: string;
  items: ConnectionItem[];
  onChanged: () => Promise<void>;
  onClose: () => void;
}) {
  const [id, setId] = useState("");
  const [connector, setConnector] = useState<Connector>("sftp");
  const [values, setValues] = useState<Record<string, string>>(DEFAULTS.sftp);
  const [removed, setRemoved] = useState(false);
  const [hostKey, setHostKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [scanned, setScanned] = useState<{ host: string; port: string; fingerprint: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const existing = items.find((c) => c.id === id);
  const fields = connectionFields(connector, values.type);
  const changeType = (type: Connector) => {
    setConnector(type);
    setValues(DEFAULTS[type]);
    setRemoved(false);
    setScanned(null);
    setHostKey("");
    setError(null);
  };

  const edit = (c: ConnectionItem) => {
    setScanned(null);
    setId(c.id);
    const type = c.connector as Connector;
    if (!Object.hasOwn(DEFAULTS, type)) return;
    setConnector(type);
    setRemoved(false);
    const auth = (c.settings.auth ?? {}) as Record<string, unknown>;
    const v = { ...DEFAULTS[type] };
    for (const f of connectionFields(type, String(auth.type ?? "none"))) {
      const value = (type === "http" && f.name !== "baseUrl" ? auth : c.settings)[f.name];
      if (!f.writeOnly && value !== undefined) v[f.name] = String(value);
    }
    setValues(v);
    setHostKey(String(c.settings["hostKeyFingerprint"] ?? c.settings["knownHosts"] ?? ""));
    setError(null);
  };

  const fetchHostKey = async () => {
    setError(null);
    setScanned(null);
    setBusy(true);
    const host = values.host ?? "";
    const port = values.port || "22";
    try {
      const { keys } = await api.scanHostKey(workspaceId, host, Number(port));
      const key = keys[0];
      if (!key) throw new Error("The server did not present a host key.");
      setScanned({ host, port, fingerprint: key.fingerprintSha256 });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const save = async () => {
    setError(null);
    if (!ID_RE.test(id)) return setError("Connection id must match ^[a-z][a-z0-9_-]*$");
    if (connector === "sftp" && !hostKey.trim()) return setError("A host key is required. Paste the server's SHA256 fingerprint or known_hosts content.");
    const settings: Record<string, unknown> = {};
