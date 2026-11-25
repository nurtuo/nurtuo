// SPDX-License-Identifier: AGPL-3.0-only
import { useState } from "react";
import type { WorkspaceInfo } from "../api/controlPlane";

export interface Session {
  email: string;
  workspaces: WorkspaceInfo[];
}

/** Sign-in and workspace picker. Offline (no control plane) the canvas works as a local editor. */
export function LoginBar({ session, workspaceId, onWorkspace, restoring, onLogin, onLogout }: {
  session: Session | null;
  workspaceId: string | null;
  onWorkspace: (id: string) => void;
  restoring: boolean;
  onLogin: (email: string, password: string) => Promise<string | null>;
  onLogout: () => Promise<string | null>;
}) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [signingIn, setSigningIn] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  if (session) {
    return (
      <span className="loginbar">
        {session.workspaces.length === 0 ? <span>No workspaces available</span> : (
          <select aria-label="Workspace" value={workspaceId ?? ""} onChange={(e) => onWorkspace(e.target.value)}>
            {session.workspaces.map((w) => <option key={w.id} value={w.id}>{w.name} ({w.role})</option>)}
          </select>
        )}
        <span title={session.email}>{session.email}</span>
        <button disabled={loggingOut} onClick={async () => {
          setErr(null);
          setLoggingOut(true);
          setErr(await onLogout());
          setLoggingOut(false);
        }}>Log out</button>
        {err && <span className="err" role="alert">{err}</span>}
      </span>
    );
  }
  return (
    <form className="loginbar" onSubmit={async (e) => {
      e.preventDefault();
      if (restoring || signingIn) return;
      setSigningIn(true);
      try {
        setErr(await onLogin(email, password));
        setPassword("");
      } finally {
        setSigningIn(false);
      }
    }}>
      <input aria-label="Email" placeholder="email" value={email} onChange={(e) => setEmail(e.target.value)} />
      <input aria-label="Password" type="password" placeholder="password" value={password} onChange={(e) => setPassword(e.target.value)} />
      <button type="submit" disabled={restoring || signingIn}>Sign in</button>
      {err && <span className="err" role="alert">{err}</span>}
    </form>
  );
}
