// SPDX-License-Identifier: AGPL-3.0-only
import { ReactFlowProvider } from "@xyflow/react";
import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { DND_MIME, STEP_CATALOG, emptyFlow } from "../model/catalog";
import { parseLoaded, serializeFlow, serializeLayout } from "../model/flowAdapter";
import { FlowStore } from "../model/flowStore";
import { ApiError, ControlPlane, describeApiError, type ConnectionItem } from "../api/controlPlane";
import { Canvas } from "./Canvas";
import { ConfigDialog } from "./ConfigDialog";
import { ConnectionsPanel } from "./ConnectionsPanel";
import { LoginBar, type Session } from "./LoginBar";

const SESSION_HINT = "nurtuo.cp.session"; // "a session cookie probably exists"; avoids probing the control plane when offline

function download(name: string, text: string) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], { type: "application/json" }));
  a.download = name;
  a.click();
  URL.revokeObjectURL(a.href);
}

export function App() {
  const store = useMemo(() => new FlowStore(emptyFlow()), []);
  const state = useSyncExternalStore(store.subscribe, store.getState);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  const cp = useMemo(() => new ControlPlane(), []);
  const [restoring, setRestoring] = useState(() => Boolean(localStorage.getItem(SESSION_HINT)));
  const [restoreError, setRestoreError] = useState<string | null>(null);
  const signingIn = useRef(false);
  const restore = useRef<ReturnType<ControlPlane["me"]> | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);
  const [connections, setConnections] = useState<ConnectionItem[] | null>(null);
  const [showConnections, setShowConnections] = useState(false);
  const listRequest = useRef<AbortController>(null);
  const currentWorkspace = useRef<string | null>(null);

  const enter = (me: { user: { email?: string } | null; workspaces: Session["workspaces"] }) => {
    setSession({ email: me.user?.email ?? "", workspaces: me.workspaces });
    currentWorkspace.current ??= me.workspaces[0]?.id ?? null;
    setWorkspaceId(currentWorkspace.current);
    localStorage.setItem(SESSION_HINT, "1");
  };
  const leave = () => {
    currentWorkspace.current = null;
    listRequest.current?.abort();
    setSession(null); setWorkspaceId(null); setConnections(null);
    setShowConnections(false);
    localStorage.removeItem(SESSION_HINT);
  };

  // Restore the session (and the CSRF token) after a reload.
  useEffect(() => {
    if (!localStorage.getItem(SESSION_HINT)) return;
    let active = true;
    restore.current ??= cp.me();
    restore.current.then((me) => {
      if (active) me ? enter(me) : leave();
    }, () => {
      if (active) setRestoreError("Could not restore your session. Try signing in again.");
    }).finally(() => { if (active) setRestoring(false); });
    return () => { active = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cp]);

  const reloadConnections = async () => {
    if (!workspaceId || workspaceId !== currentWorkspace.current) return;
    listRequest.current?.abort();
    const request = new AbortController();
    listRequest.current = request;
    try {
      const items = await cp.listConnections(workspaceId, request.signal);
      if (!request.signal.aborted) setConnections(items);
    } catch {
      if (!request.signal.aborted) setConnections(null);
    }
  };
  useEffect(() => {
    void reloadConnections();
    return () => listRequest.current?.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspaceId]);

  const onLogin = async (email: string, password: string): Promise<string | null> => {
    if (restoring || signingIn.current) return null;
    signingIn.current = true;
    setRestoreError(null);
    try {
      const me = await cp.login(email, password);
      if (!me) return "Sign-in failed";
      enter(me);
      return null;
    } catch (e) {
      return e instanceof ApiError ? e.message : describeApiError(e);
    } finally {
      signingIn.current = false;
    }
  };
  const onLogout = async () => {
    try {
      await cp.logout();
      leave();
      return null;
    } catch {
      return "Could not revoke the session. Try logging out again.";
    }
  };

  const changeWorkspace = (id: string) => {
    currentWorkspace.current = id;
    listRequest.current?.abort();
    setWorkspaceId(id); setConnections(null);
    setShowConnections(false);
  };

  const workspace = session?.workspaces.find((w) => w.id === workspaceId) ?? null;

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT") return;
      if (!(e.ctrlKey || e.metaKey)) return;
      const k = e.key.toLowerCase();
      if (k === "z" && !e.shiftKey) store.undo();
      else if ((k === "z" && e.shiftKey) || k === "y") store.redo();
      else return;
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [store]);

  // Undo/redo can remove the selected step.
  const selected = state.flow.steps.find((s) => s.id === selectedId);
  const editStep = state.flow.steps.find((s) => s.id === editing);

  const onLoad = async (file: File) => {
    try {
      const { flow, layout } = parseLoaded(await file.text());
      if (flow) store.load(flow, layout?.positions);
      else if (layout) store.setPositions(layout.positions);
      setLoadError(null);
      setSelectedId(null);
      setEditing(null);
    } catch (e) {
      setLoadError(String(e instanceof Error ? e.message : e));
    }
  };

  const errors = state.diagnostics.filter((d) => d.severity === "error").length;

  return (
    <div className="app">
      <div className="toolbar">
        <button disabled={!state.canUndo} onClick={() => store.undo()}>Undo</button>
        <button disabled={!state.canRedo} onClick={() => store.redo()}>Redo</button>
        <span className="sep" />
        <button onClick={() => fileRef.current?.click()}>Load JSON</button>
        <button onClick={() => download(`${state.flow.id}.flow.json`, serializeFlow(state.flow))}>Save flow</button>
        <button onClick={() => download(`${state.flow.id}.layout.json`, serializeLayout(state.flow.id, state.positions))}>Save layout</button>
        <span className="sep" />
        <button disabled={!session || !workspace} title={session ? "" : "Sign in to manage connections"} onClick={() => setShowConnections(true)}>Connections</button>
        <span className="sep" />
        <LoginBar session={session} workspaceId={workspaceId} restoring={restoring} onLogin={onLogin} onLogout={onLogout}
          onWorkspace={changeWorkspace} />
        {restoreError && <span className="err" role="alert">{restoreError}</span>}
        <input ref={fileRef} type="file" accept="application/json,.json" hidden
          onChange={(e) => { const f = e.target.files?.[0]; if (f) void onLoad(f); e.target.value = ""; }} />
        {loadError && <span className="err">{loadError}</span>}
      </div>
      <aside className="palette">
        <strong>Steps</strong>
        <div style={{ height: 8 }} />
        {STEP_CATALOG.map((s) => (
          <div
            key={s.kind}
            className="palette-item"
            title={s.description}
            draggable
            onDragStart={(e) => {
              e.dataTransfer.setData(DND_MIME, s.kind);
              e.dataTransfer.effectAllowed = "move";
            }}
          >
            {s.label}
          </div>
        ))}
      </aside>
      <main className="canvas">
        <ReactFlowProvider>
          <Canvas store={store} state={state} selectedId={selectedId} onSelect={setSelectedId} onEdit={setEditing} />
        </ReactFlowProvider>
      </main>
      <aside className="panel">
        <strong>Step</strong>
        {selected ? (
          <p>
            {selected.label ?? selected.kind} <code>{selected.id}</code>{" "}
            <button onClick={() => setEditing(selected.id)}>Configure</button>
          </p>
        ) : (
          <p>Select a step (double-click to configure).</p>
        )}
        <strong>Diagnostics ({errors} errors, {state.diagnostics.length - errors} warnings)</strong>
        <ul className="diagnostics">
          {state.diagnostics.map((d, i) => (
            <li key={i} className={d.severity} onClick={() => d.stepId && setSelectedId(d.stepId)}>
              <code>{d.code}</code> {d.message}
              {d.stepId && <em> [{d.stepId}]</em>}
            </li>
          ))}
          {state.diagnostics.length === 0 && <li>No issues.</li>}
        </ul>
      </aside>
      {showConnections && workspaceId && (
        <ConnectionsPanel api={cp} workspaceId={workspaceId} items={connections ?? []} onChanged={reloadConnections} onClose={() => setShowConnections(false)} />
      )}
      {editStep && (
        <ConfigDialog connections={connections} store={store} step={editStep} onClose={() => setEditing(null)}
          onRenamed={(id) => { setEditing(id); setSelectedId(id); }} />
      )}
    </div>
  );
}
