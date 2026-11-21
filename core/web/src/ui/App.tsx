// SPDX-License-Identifier: AGPL-3.0-only
import { ReactFlowProvider } from "@xyflow/react";
import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { DND_MIME, STEP_CATALOG, emptyFlow } from "../model/catalog";
import { parseLoaded, serializeFlow, serializeLayout } from "../model/flowAdapter";
import { FlowStore } from "../model/flowStore";
import { Canvas } from "./Canvas";
import { ConfigDialog } from "./ConfigDialog";

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
      {editStep && (
        <ConfigDialog store={store} step={editStep} onClose={() => setEditing(null)}
          onRenamed={(id) => { setEditing(id); setSelectedId(id); }} />
      )}
    </div>
  );
}
