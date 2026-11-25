// SPDX-License-Identifier: AGPL-3.0-only
import { ReactFlowProvider } from "@xyflow/react";
import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { DND_MIME, STEP_CATALOG, emptyFlow } from "../model/catalog";
import { parseLoaded, serializeFlow, serializeLayout } from "../model/flowAdapter";
import { FlowStore } from "../model/flowStore";
import { ApiError, ControlPlane, OfflineError, describeApiError, type ConnectionItem } from "../api/controlPlane";
import { IDLE, hashFlow, reconcile, stepStatuses, type RunState } from "../model/runState";
import { Canvas } from "./Canvas";
import { ConfigDialog } from "./ConfigDialog";
import { ConnectionsPanel } from "./ConnectionsPanel";
import { LoginBar, type Session } from "./LoginBar";
import { ResultsPanel, RunDialog } from "./RunPanel";

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
  const [online, setOnline] = useState(true);
  const [restoring, setRestoring] = useState(() => Boolean(localStorage.getItem(SESSION_HINT)));
  const signingIn = useRef(false);
  const restore = useRef<ReturnType<ControlPlane["me"]> | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);
  const [connections, setConnections] = useState<ConnectionItem[] | null>(null);
  const [showConnections, setShowConnections] = useState(false);
  const [showRun, setShowRun] = useState(false);
  const [run, setRun] = useState<RunState>(IDLE);
  const listRequest = useRef<AbortController>(null);
  const runRequest = useRef<AbortController>(null);
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
    runRequest.current?.abort();
    setSession(null); setWorkspaceId(null); setConnections(null); setRun(IDLE);
    setShowRun(false); setShowConnections(false);
    localStorage.removeItem(SESSION_HINT);
  };

  // Restore the session (and the CSRF token) after a reload.
  useEffect(() => {
    if (!localStorage.getItem(SESSION_HINT)) return;
    let active = true;
    restore.current ??= cp.me();
    restore.current.then((me) => {
      if (active) me ? enter(me) : leave();
    }, (error) => {
      if (active && error instanceof OfflineError) setOnline(false);
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
    try {
      const me = await cp.login(email, password);
      if (!me) return "Sign-in failed";
      setOnline(true);
      enter(me);
      return null;
    } catch (e) {
      if (e instanceof OfflineError) setOnline(false);
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
    runRequest.current?.abort();
    setWorkspaceId(id); setConnections(null); setRun(IDLE);
    setShowRun(false); setShowConnections(false);
  };

  const workspace = session?.workspaces.find((w) => w.id === workspaceId) ?? null;
  const canRun = workspace?.permissions.includes("test_run:execute") ?? false;

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

  // Any edit invalidates the run colours and results.
  useEffect(() => setRun((r) => reconcile(r, state.flow)), [state.flow]);
  const runStatus = useMemo(() => stepStatuses(run, state.flow), [run, state.flow]);

  const runDisabledReason = !online ? "Control plane unreachable"
    : !session ? "Sign in to run flows"
    : !workspace ? "Select a workspace"
    : !canRun ? "Your role cannot run flows"
    : errors > 0 ? "Fix the flow errors first"
    : run.phase === "running" ? "A run is in progress" : null;

  const doRun = async (properties: Record<string, string>, timeoutMs: number) => {
    if (!workspaceId) return;
    setShowRun(false);
    const flow = state.flow;
    const flowHash = hashFlow(flow);
    const activeRun = { phase: "running", flowHash } as const;
    runRequest.current?.abort();
    const request = new AbortController();
    runRequest.current = request;
    setRun(activeRun);
    try {
      const result = await cp.testRun(workspaceId, { flow, input: { properties }, timeoutMs }, request.signal);
      setRun((r) => (r === activeRun ? { phase: "done", flowHash, result } : r));
    } catch (e) {
      if (request.signal.aborted) return;
      if (e instanceof ApiError && e.status === 401) leave();
      if (e instanceof OfflineError) setOnline(false);
      setRun((r) => (r === activeRun ? { phase: "error", flowHash, message: describeApiError(e) } : r));
    }
  };

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
        <button disabled={!online || !session || !workspace} title={session ? "" : "Sign in to manage connections"} onClick={() => setShowConnections(true)}>Connections</button>
        <button disabled={runDisabledReason !== null} title={runDisabledReason ?? "Run this flow against your workspace connections"} onClick={() => setShowRun(true)}>Run</button>
        <span className="sep" />
        <LoginBar session={session} workspaceId={workspaceId} online={online} restoring={restoring} onLogin={onLogin} onLogout={onLogout}
          onWorkspace={changeWorkspace} />
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
          <Canvas store={store} state={state} selectedId={selectedId} onSelect={setSelectedId} onEdit={setEditing} runStatus={runStatus} />
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
        {runDisabledReason && !session && <p className="hint">Run needs a control plane: {runDisabledReason.toLowerCase()}.</p>}
        <ResultsPanel run={run} onSelect={setSelectedId} />
      </aside>
      {showRun && <RunDialog flow={state.flow} onRun={(p, t) => void doRun(p, t)} onClose={() => setShowRun(false)} />}
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
