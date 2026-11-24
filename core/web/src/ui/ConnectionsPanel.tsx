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
    if (connector === "sftp" && !hostKey.trim()) return setError("Paste the server's SHA256 fingerprint or known_hosts content.");
    const settings: Record<string, unknown> = {};
    const auth: Record<string, unknown> = {};
    const credentials: Record<string, string | null> = {};
    for (const f of fields) {
      const raw = values[f.name] ?? "";
      if (f.writeOnly) {
        if (removed && connector === "postgres") credentials[f.name] = null;
        else if (raw) credentials[f.name] = raw;
        else if ((connector === "http" && !existing?.secrets[f.name]) || (connector === "sftp" && !existing)) return setError(`${f.name} is required`);
        continue;
      }
      if (f.required && !raw.trim()) return setError(`${f.name} is required`);
      if (!raw && f.name !== "rootPath") continue;
      const target = connector === "http" && f.name !== "baseUrl" ? auth : settings;
      target[f.name] = f.type === "integer" || f.type === "number" ? Number(raw) : raw;
    }
    if (connector === "http") {
      settings.auth = auth;
      const active = fields.find((f) => f.writeOnly)?.name;
      for (const field of Object.keys(existing?.secrets ?? {})) {
        if (field !== active) credentials[field] = null;
      }
    }
    if (connector === "sftp") {
      if (hostKey.trim().startsWith("SHA256:")) {
        settings.hostKeyPolicy = "fingerprint";
        settings.hostKeyFingerprint = hostKey.trim();
      } else settings.knownHosts = hostKey;
    }
    setBusy(true);
    try {
      await api.putConnection(workspaceId, id, { connector, settings, credentials });
      await onChanged();
      setRemoved(false);
      setValues((p) => ({ ...p, password: "", token: "", apiKeyValue: "" }));
    } catch (e) {
      setError(e instanceof ApiError ? `${e.message} (${e.code})` : String(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (c: ConnectionItem) => {
    try {
      await api.deleteConnection(workspaceId, c.id);
      await onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="dialog" role="dialog" aria-label="Connections">
        <h3>Connections</h3>
        <ul className="diagnostics">
          {items.map((c) => (
            <li key={c.id} data-connection={c.id} onClick={() => edit(c)}>
              <code>{c.id}</code> ({c.connector}) {String(c.settings["host"] ?? "")}{" "}
              {Object.entries(c.secrets).map(([k, v]) => <span key={k} className={`badge ${v}`}>{k}: {v}</span>)}{" "}
              <button onClick={(e) => { e.stopPropagation(); void remove(c); }}>Delete</button>
            </li>
          ))}
          {items.length === 0 && <li>No connections yet.</li>}
        </ul>
        <strong>{existing ? `Edit ${id}` : "New connection"}</strong>
        <label className="field"><span>id *</span><input aria-label="Connection id" value={id} onChange={(e) => {
          const item = items.find((c) => c.id === e.target.value);
          if (item) edit(item);
          else setId(e.target.value);
        }} /></label>
        <label className="field"><span>Connector</span>
          <select value={connector} disabled={!!existing} onChange={(e) => changeType(e.target.value as Connector)}>
            <option value="sftp">SFTP</option>
            <option value="http">HTTP</option>
            <option value="postgres">PostgreSQL</option>
          </select>
        </label>
        {fields.map((f) => (
          <label className="field" key={f.name} title={f.description ?? f.name}>
            <span>{f.name}{f.required || f.writeOnly ? " *" : ""}</span>
            {f.type === "enum" ? (
              <select aria-label={f.name} value={values[f.name] ?? ""} onChange={(e) => {
                const value = e.target.value;
                setValues((p) => f.name === "type"
                  ? { ...p, type: value, password: "", token: "", apiKeyValue: "" }
                  : { ...p, [f.name]: value });
              }}>
                {f.options?.map((option) => <option key={String(option)}>{String(option)}</option>)}
              </select>
            ) : <input
              aria-label={f.name}
              disabled={f.writeOnly && removed}
              type={f.writeOnly ? "password" : f.type === "integer" || f.type === "number" ? "number" : "text"}
              autoComplete={f.writeOnly ? "new-password" : "off"}
              value={values[f.name] ?? ""}
              placeholder={f.writeOnly && existing ? "unchanged (set a new value to replace)" : ""}
              onChange={(e) => setValues((p) => ({ ...p, [f.name]: e.target.value }))}
            />}
          </label>
        ))}
        {connector === "postgres" && existing?.secrets.password && (
          <label><input type="checkbox" checked={removed} onChange={(e) => setRemoved(e.target.checked)} /> Remove stored password</label>
        )}
        {connector === "sftp" && <fieldset>
          <legend>Host key</legend>
          <textarea aria-label="Host key" rows={3} value={hostKey} onChange={(e) => setHostKey(e.target.value)}
            placeholder="SHA256:… or a known_hosts entry" />
          <button disabled={busy || !values.host} onClick={() => void fetchHostKey()}>Fetch host key</button>
          {scanned && scanned.host === values.host && scanned.port === (values.port || "22") && (
            <div>
              <code>{scanned.fingerprint}</code>
              <button disabled={busy} onClick={() => {
                setHostKey(scanned.fingerprint);
                setScanned(null);
              }}>Trust this key</button>
            </div>
          )}
          <small>Compare with the fingerprint published by the server's administrator. There is no option to skip verification.</small>
        </fieldset>}
        {error && <p className="err" role="alert">{error}</p>}
        <button disabled={busy} onClick={() => void save()}>Save connection</button>{" "}
        <button onClick={onClose}>Close</button>
      </div>
    </div>
  );
}
