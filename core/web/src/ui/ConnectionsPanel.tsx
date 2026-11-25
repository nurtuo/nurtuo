// SPDX-License-Identifier: AGPL-3.0-only
import { useState } from "react";
import sftpSchema from "../../../../connectors/sftp/schemas/connection.json";
import { ApiError, type ConnectionItem, type ControlPlane } from "../api/controlPlane";
import { fieldsOf, type Field } from "../model/formSchema";

// Generated from the connector's own connection schema. This slice offers password authentication
// and a pinned host key only: no privateKey/passphrase, no `insecure`, no oneOf handling.
const HIDDEN = new Set(["privateKey", "passphrase", "hostKeyPolicy", "knownHosts", "hostKeyFingerprint", "maxListEntries", "timeoutMs"]);
const FIELDS: Field[] = fieldsOf(sftpSchema as Record<string, unknown>).filter((f) => !HIDDEN.has(f.name));
const ID_RE = /^[a-z][a-z0-9_-]*$/;

/** Workspace connections: list, create/edit an SFTP connection, delete. Credential values are write-only. */
export function ConnectionsPanel({ api, workspaceId, items, onChanged, onClose }: {
  api: ControlPlane;
  workspaceId: string;
  items: ConnectionItem[];
  onChanged: () => Promise<void>;
  onClose: () => void;
}) {
  const [id, setId] = useState("");
  const [values, setValues] = useState<Record<string, string>>({ port: "22" });
  const [hostKey, setHostKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const existing = items.find((c) => c.id === id);

  const edit = (c: ConnectionItem) => {
    setId(c.id);
    const v: Record<string, string> = {};
    for (const f of FIELDS) if (!f.writeOnly && c.settings[f.name] !== undefined) v[f.name] = String(c.settings[f.name]);
    setValues(v); // the password field stays empty: values are never readable
    setHostKey(String(c.settings["hostKeyFingerprint"] ?? c.settings["knownHosts"] ?? ""));
    setError(null);
  };

  const save = async () => {
    setError(null);
    if (!ID_RE.test(id)) return setError("Connection id must match ^[a-z][a-z0-9_-]*$");
    if (!values["host"] || !values["user"]) return setError("host and user are required");
    if (!hostKey.trim()) return setError("A host key is required: paste the server's SHA256 fingerprint or known_hosts content.");
    const typed = FIELDS.filter((f) => f.writeOnly).map((f) => [f, values[f.name] ?? ""] as const).filter(([, v]) => v !== "");
    const settings: Record<string, unknown> = {};
    for (const f of FIELDS) {
      if (f.writeOnly) continue;
      const raw = values[f.name];
      if (raw === undefined || (raw === "" && f.name !== "rootPath")) continue;
      settings[f.name] = f.type === "integer" || f.type === "number" ? Number(raw) : raw;
    }
    if (typed.length === 0 && !existing) return setError("password is required");
    if (hostKey.trim().startsWith("SHA256:")) {
      settings["hostKeyPolicy"] = "fingerprint";
      settings["hostKeyFingerprint"] = hostKey.trim();
    }
    else settings["knownHosts"] = hostKey;
    setBusy(true);
    try {
      const credentials = typed.length ? Object.fromEntries(typed.map(([f, value]) => [f.name, value])) : undefined;
      await api.putConnection(workspaceId, id, { connector: "sftp", settings, credentials });
      await onChanged();
      setValues((p) => ({ ...p, password: "" }));
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
        <strong>{existing ? `Edit ${id}` : "New SFTP connection"}</strong>
        <label className="field"><span>id *</span><input aria-label="Connection id" value={id} onChange={(e) => setId(e.target.value)} /></label>
        {FIELDS.map((f) => (
          <label className="field" key={f.name} title={f.description ?? f.name}>
            <span>{f.name}{f.required || f.writeOnly ? " *" : ""}</span>
            <input
              aria-label={f.name}
              type={f.writeOnly ? "password" : f.type === "integer" || f.type === "number" ? "number" : "text"}
              autoComplete={f.writeOnly ? "new-password" : "off"}
              value={values[f.name] ?? ""}
              placeholder={f.writeOnly && existing ? "unchanged (set a new value to replace)" : ""}
              onChange={(e) => setValues((p) => ({ ...p, [f.name]: e.target.value }))}
            />
          </label>
        ))}
        <fieldset>
          <legend>Host key</legend>
          <textarea aria-label="Host key" rows={3} value={hostKey} onChange={(e) => setHostKey(e.target.value)}
            placeholder="SHA256:… or a known_hosts entry" />
          <small>Compare with the fingerprint published by the server's administrator. There is no option to skip verification.</small>
        </fieldset>
        {error && <p className="err" role="alert">{error}</p>}
        <button disabled={busy} onClick={() => void save()}>Save connection</button>{" "}
        <button onClick={onClose}>Close</button>
      </div>
    </div>
  );
}
