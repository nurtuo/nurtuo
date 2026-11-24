// SPDX-License-Identifier: AGPL-3.0-only
import { useState } from "react";
import type { Flow } from "@nurtuo/flow-schema";
import type { RunDocument } from "../api/controlPlane";
import { STATUS_LABEL, failureHint, previewOf, decodeBase64, type RunState } from "../model/runState";

export const hasSftpGet = (flow: Flow): boolean =>
  flow.steps.some((s) => s.kind === "connector" && s.config["connector"] === "sftp" && String(s.config["operation"] ?? "").toUpperCase() === "GET");

type Row = { key: string; value: string };

/** Run dialog: input properties (sent as the initial document's properties, like --set) and timeout. */
export function RunDialog({ flow, onRun, onClose }: {
  flow: Flow;
  onRun: (properties: Record<string, string>, timeoutMs: number) => void;
  onClose: () => void;
}) {
  const [rows, setRows] = useState<Row[]>(hasSftpGet(flow) ? [{ key: "path", value: "" }] : []);
  const [timeout, setTimeoutS] = useState("30");
  const set = (i: number, r: Partial<Row>) => setRows((p) => p.map((x, j) => (j === i ? { ...x, ...r } : x)));
  const secs = Number(timeout);
  const bad = !Number.isFinite(secs) || secs < 1 || secs > 120;
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="dialog" role="dialog" aria-label="Run flow">
        <h3>Run flow</h3>
        <strong>Input properties</strong>
        {rows.map((r, i) => (
          <div key={i} className="row">
            <input aria-label={`property name ${i}`} value={r.key} onChange={(e) => set(i, { key: e.target.value })} />
            <span>=</span>
            <input aria-label={`property value ${i}`} value={r.value} placeholder={r.key === "path" ? "/readme.txt" : ""} onChange={(e) => set(i, { value: e.target.value })} />
            <button onClick={() => setRows((p) => p.filter((_, j) => j !== i))}>×</button>
          </div>
        ))}
        <button onClick={() => setRows((p) => [...p, { key: "", value: "" }])}>Add property</button>
        <label className="field"><span>timeout (seconds, 1–120)</span>
          <input aria-label="timeout" type="number" min={1} max={120} value={timeout} onChange={(e) => setTimeoutS(e.target.value)} />
        </label>
        <button disabled={bad} onClick={() => onRun(Object.fromEntries(rows.filter((r) => r.key.trim() !== "").map((r) => [r.key.trim(), r.value])), Math.round(secs * 1000))}>
          Run
        </button>{" "}
        <button onClick={onClose}>Cancel</button>
      </div>
    </div>
  );
}

function download(doc: RunDocument) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([decodeBase64(doc.payload) as BlobPart]));
  a.download = doc.properties["filename"] || `${doc.id}.bin`;
  a.click();
  URL.revokeObjectURL(a.href);
}

/** Results section of the side panel. Text is rendered as React text nodes (never innerHTML). */
export function ResultsPanel({ run, onSelect }: { run: RunState; onSelect: (id: string) => void }) {
  if (run.phase === "idle") return null;
  if (run.phase === "running") return <section aria-label="Run result"><strong>Run</strong><p>Running…</p></section>;
  if (run.phase === "error") return <section aria-label="Run result"><strong>Run</strong><p className="err" role="alert">{run.message}</p></section>;
  const r = run.result;
  const hint = failureHint(r);
  return (
    <section aria-label="Run result">
      <strong>Run</strong>
      <p>
        <span data-testid="run-status" className={`run-badge ${r.status}`}>{r.status}</span> {r.durationMs} ms
      </p>
      {r.error && <p className="err">{r.error.message}</p>}
      {hint && <p className="hint">{hint}</p>}
      {r.stop && <p>Stopped{r.stop.status !== undefined ? ` with status ${r.stop.status}` : ""}{r.stop.message ? `: ${r.stop.message}` : ""}</p>}
      <ul className="diagnostics">
        {r.steps.map((s) => (
          <li key={s.id} onClick={() => onSelect(s.id)}><code>{s.id}</code> {STATUS_LABEL[s.status]} (in {s.in}, out {s.out}, failed {s.failed})</li>
        ))}
      </ul>
      {r.documents?.map((d) => {
        const p = previewOf(d);
        return (
          <div key={d.id} className="doc">
            <code>{d.id}</code>{" "}
            <button onClick={() => download(d)}>Download</button>
            {p.kind === "text"
              ? <pre data-testid="doc-preview">{p.text}{p.truncated ? "\n… (truncated)" : ""}</pre>
              : <p data-testid="doc-preview">binary, {p.size} bytes</p>}
          </div>
        );
      })}
    </section>
  );
}
