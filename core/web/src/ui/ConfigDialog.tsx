// SPDX-License-Identifier: AGPL-3.0-only
import { STEP_ID_PATTERN, type Step } from "@nurtuo/flow-schema";
import { useState } from "react";
import type { ConnectionItem } from "../api/controlPlane";
import { configSchemaOf } from "../model/catalog";
import { configureWorkspaceConnector, workspaceOperations } from "../model/connectorOperations";
import { fieldsOf, fromText, toText, withField, type Field } from "../model/formSchema";
import { configPatch, type FlowStore } from "../model/flowStore";

/** Modal form generated from the step kind's config JSON Schema. Edits go through the CommandBus. */
export function ConfigDialog({ store, step, onClose, onRenamed, connections }: {
  /** Workspace connections for the `connection` picker; null/undefined offline (free text). */
  connections?: ConnectionItem[] | null;
  store: FlowStore;
  step: Step;
  onClose: () => void;
  onRenamed: (id: string) => void;
}) {
  const fields = fieldsOf(configSchemaOf(step.kind));
  const connector = step.config.connector;
  const operation = String(step.config.operation ?? "");
  const bound = connections?.find((c) => c.id === step.config.connection && c.connector === connector);
  const allowed = step.kind === "connector" ? workspaceOperations(bound?.connector) : undefined;
  const operations = step.config.objectType === undefined && (!operation || allowed?.includes(operation.toUpperCase()))
    ? allowed?.map((choice) => choice === operation.toUpperCase() ? operation : choice)
    : undefined;
  const connectionChoices = connections?.filter((c) => !connector || c.connector === connector
    || (!!workspaceOperations(connector) && !!workspaceOperations(c.connector)));
  const [error, setError] = useState<string | null>(null);

  const run = (ops: Parameters<FlowStore["dispatch"]>[0]) => {
    const r = store.dispatch(ops);
    setError(r.ok ? null : r.message);
    return r.ok;
  };

  const commit = (f: Field, text: string): string | null => {
    const p = fromText(f, text);
    if (!p.ok) return p.error;
    const selected = f.name === "connection" ? connections?.find((c) => c.id === p.value) : undefined;
    const next = selected ? configureWorkspaceConnector(step.config, selected) : withField(step.config, f.name, p.value);
    const patch = configPatch(step.config, next);
    if (Object.keys(patch).length > 0) run([{ op: "updateConfig", id: step.id, patch }]);
    return null;
  };

  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="dialog" role="dialog" aria-label={`Configure ${step.id}`}>
        <h3>
          {step.kind} <code>{step.id}</code>
        </h3>
        <label className="field">
          <span>id</span>
          <input
            key={`id-${step.id}`}
            defaultValue={step.id}
            onBlur={(e) => {
              const v = e.target.value;
              if (v === step.id) return;
              if (!STEP_ID_PATTERN.test(v)) return setError(`Invalid id ${v}`);
              if (run([{ op: "renameStep", id: step.id, newId: v }])) onRenamed(v);
            }}
          />
        </label>
        <label className="field">
          <span>label</span>
          <input
            key={`label-${step.label ?? ""}`}
            defaultValue={step.label ?? ""}
            onBlur={(e) => {
              const v = e.target.value;
              if (v !== (step.label ?? "")) run([{ op: "setLabel", id: step.id, label: v === "" ? null : v }]);
            }}
          />
        </label>
        {fields.filter((f) => !operations || (f.name !== "connector" && f.name !== "objectType")).map((f) => (
          // key includes the stored value so undo/redo resets the editor text
          <FieldEditor key={`${step.id}:${f.name}:${JSON.stringify(step.config[f.name])}`} field={f} value={step.config[f.name]} onCommit={(t) => commit(f, t)}
            choices={step.kind === "connector" && f.name === "connection"
              ? (connectionChoices?.length ? connectionChoices.map((c) => c.id) : undefined)
              : step.kind === "connector" && f.name === "operation" ? operations : undefined} />
        ))}
        {fields.length === 0 && <p>This step has no configuration.</p>}
        {error && <p className="err">{error}</p>}
        <button onClick={onClose}>Close</button>
      </div>
    </div>
  );
}

function FieldEditor({ field, value, onCommit, choices }: { field: Field; value: unknown; onCommit: (text: string) => string | null; choices?: readonly string[] }) {
  const [text, setText] = useState(toText(field, value));
  const [err, setErr] = useState<string | null>(null);
  const commit = (t: string) => setErr(onCommit(t));
  const title = field.description ?? field.name;
  let control;
  if (choices) {
    const cur = typeof value === "string" && value !== "" && !choices.includes(value) ? [value] : [];
    control = (
      <select aria-label={field.name} value={text} onChange={(e) => { setText(e.target.value); commit(e.target.value); }}>
        {(!field.required || !text) && <option value="" disabled={field.required} />}
        {[...cur, ...choices].map((c) => <option key={c} value={c}>{c}</option>)}
      </select>
    );
  } else if (field.type === "boolean") {
    control = <input type="checkbox" checked={value === true} onChange={(e) => commit(String(e.target.checked))} />;
  } else if (field.type === "enum") {
    control = (
      <select value={text} onChange={(e) => { setText(e.target.value); commit(e.target.value); }}>
        {!field.required && <option value="" />}
        {field.options?.map((o) => <option key={String(o)} value={String(o)}>{String(o)}</option>)}
      </select>
    );
  } else if (field.type === "json") {
    control = <textarea rows={5} value={text} onChange={(e) => setText(e.target.value)} onBlur={() => commit(text)} />;
  } else {
    control = (
      <input
        type={field.type === "string" ? "text" : "number"}
        {...(field.min !== undefined ? { min: field.min } : {})}
        {...(field.max !== undefined ? { max: field.max } : {})}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onBlur={() => commit(text)}
      />
    );
  }
  return (
    <label className="field" title={title}>
      <span>{field.name}{field.required ? " *" : ""}</span>
      {control}
      {err && <span className="err">{err}</span>}
    </label>
  );
}
