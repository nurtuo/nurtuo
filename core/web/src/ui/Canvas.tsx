// SPDX-License-Identifier: AGPL-3.0-only
import {
  Background,
  Controls,
  Handle,
  Position,
  ReactFlow,
  SelectionMode,
  applyNodeChanges,
  useReactFlow,
  type Connection,
  type Edge,
  type Node,
  type NodeChange,
  type NodeProps,
} from "@xyflow/react";
import { STEP_KINDS, type StepKind } from "@nurtuo/flow-schema";
import { useCallback, useEffect, useMemo, useState, type DragEvent } from "react";
import { DND_MIME, GRID, newStep } from "../model/catalog";
import { checkConnection } from "../model/connection";
import { flowToGraph, type StepNodeData } from "../model/flowAdapter";
import type { FlowState, FlowStore } from "../model/flowStore";

type Severity = "error" | "warning";
type RfData = StepNodeData & { severity?: Severity; title: string };

function StepNode({ id, data, selected }: NodeProps<Node<RfData>>) {
  const ports = data.ports;
  return (
    <div className={`step-node${data.severity ? ` ${data.severity}` : ""}${selected ? " sel" : ""}`}>
      {data.kind !== "start" && <Handle type="target" position={Position.Top} />}
      <div className="step-kind">{data.kind}</div>
      <div>{data.title}</div>
      <div className="step-id">{id}</div>
      {ports.map((p, i) => (
        <Handle
          key={p}
          id={p}
          type="source"
          position={Position.Bottom}
          style={{ left: `${((i + 1) / (ports.length + 1)) * 100}%` }}
        >
          {ports.length > 1 && <span className="port-label">{p}</span>}
        </Handle>
      ))}
    </div>
  );
}

const nodeTypes = { step: StepNode };

interface Props {
  store: FlowStore;
  state: FlowState;
  selectedId: string | null;
  onSelect: (id: string | null) => void;
  onEdit: (id: string) => void;
}

function severityByStep(state: FlowState): Map<string, Severity> {
  const m = new Map<string, Severity>();
  for (const d of state.diagnostics) {
    if (!d.stepId) continue;
    if (d.severity === "error" || !m.has(d.stepId)) m.set(d.stepId, d.severity);
  }
  return m;
}

export function Canvas({ store, state, selectedId, onSelect, onEdit }: Props) {
  const { screenToFlowPosition } = useReactFlow();
  // fit-to-view is for opening an existing flow; on an empty canvas it would zoom/re-center on the first dropped step.
  const [startsNonEmpty] = useState(() => state.flow.steps.length > 0);
  const sev = useMemo(() => severityByStep(state), [state]);
  const graph = useMemo(() => flowToGraph(state.flow, state.positions), [state.flow, state.positions]);

  const project = useCallback(
    (prev: Node<RfData>[]): Node<RfData>[] => {
      // Selection requested from outside the canvas (e.g. clicking a diagnostic) that React Flow does
      // not know about yet: apply it exclusively. Otherwise keep React Flow's own (multi-)selection.
      const external = selectedId !== null && !prev.some((p) => p.id === selectedId && p.selected);
      return graph.nodes.map((n) => {
        const old = prev.find((p) => p.id === n.id);
        const severity = sev.get(n.id);
        return {
          ...(old ?? {}), // keeps measured size and selection, avoiding re-measure flicker
          id: n.id,
          type: "step",
          position: n.position,
          selected: external ? n.id === selectedId : (old?.selected ?? false),
          data: { ...n.data, title: n.data.label ?? n.data.kind, ...(severity ? { severity } : {}) },
        };
      });
    },
    [graph, sev, selectedId],
  );

  const [rfNodes, setRfNodes] = useState<Node<RfData>[]>(() => project([]));
  useEffect(() => setRfNodes((prev) => project(prev)), [project]);

  const edges: Edge[] = useMemo(
    () => graph.edges.map((e) => ({ id: e.id, source: e.source, sourceHandle: e.sourceHandle, target: e.target })),
    [graph],
  );

  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      const kept = changes.filter((c) => c.type !== "remove");
      setRfNodes((nds) => applyNodeChanges(kept, nds) as Node<RfData>[]);
      for (const c of kept) {
        if (c.type === "select") {
          if (c.selected) onSelect(c.id);
          else if (c.id === selectedId) onSelect(null); // deselected via ctrl-click / marquee
        }
      }
    },
    [onSelect, selectedId],
  );

  const onNodeDragStop = useCallback(
    (_: unknown, __: Node, nodes: Node[]) => {
      store.setPositions(Object.fromEntries(nodes.map((n) => [n.id, { x: n.position.x, y: n.position.y }])));
    },
    [store],
  );

  const portOf = (c: { source: string; sourceHandle?: string | null }) => c.sourceHandle ?? "default";

  const isValidConnection = useCallback(
    (c: Connection | Edge) =>
      checkConnection(store.getState().flow, { source: c.source, port: portOf(c), target: c.target }).ok,
    [store],
  );

  const onConnect = useCallback(
    (c: Connection) => {
      store.dispatch([{ op: "connect", from: c.source, port: portOf(c), to: c.target }]);
    },
    [store],
  );

  const onDelete = useCallback(
    ({ nodes, edges: es }: { nodes: Node[]; edges: Edge[] }) => {
      const gone = new Set(nodes.map((n) => n.id));
      const ops = [
        ...es
          .filter((e) => !gone.has(e.source) && !gone.has(e.target))
          .map((e) => ({ op: "disconnect" as const, from: e.source, port: e.sourceHandle ?? "default" })),
        ...nodes.map((n) => ({ op: "removeStep" as const, id: n.id })),
      ];
      if (ops.length > 0) store.dispatch(ops);
      onSelect(null);
    },
    [store, onSelect],
  );

  const onDrop = useCallback(
    (ev: DragEvent) => {
      ev.preventDefault();
      const kind = ev.dataTransfer.getData(DND_MIME) as StepKind;
      if (!(STEP_KINDS as readonly string[]).includes(kind)) return;
      const p = screenToFlowPosition({ x: ev.clientX, y: ev.clientY });
      const snap = (v: number) => Math.round(v / GRID) * GRID;
      const step = newStep(store.getState().flow, kind);
      store.setPositions({ [step.id]: { x: snap(p.x), y: snap(p.y) } });
      const r = store.dispatch([{ op: "addStep", step }]);
      if (r.ok) onSelect(step.id);
    },
    [screenToFlowPosition, store, onSelect],
  );

  return (
    <ReactFlow
      nodes={rfNodes}
      edges={edges}
      nodeTypes={nodeTypes}
      onNodesChange={onNodesChange}
      onNodeDragStop={onNodeDragStop}
      onNodeDoubleClick={(_, n) => onEdit(n.id)}
      onConnect={onConnect}
      isValidConnection={isValidConnection}
      onDelete={onDelete}
      onDrop={onDrop}
      onDragOver={(e) => {
        e.preventDefault();
        e.dataTransfer.dropEffect = "move";
      }}
      onPaneClick={() => onSelect(null)}
      snapToGrid
      snapGrid={[GRID, GRID]}
      selectionOnDrag
      panOnDrag={[1, 2]}
      selectionMode={SelectionMode.Partial}
      deleteKeyCode={["Backspace", "Delete"]}
      fitView={startsNonEmpty}
      fitViewOptions={{ maxZoom: 1 }}
    >
      <Background gap={GRID} />
      <Controls />
    </ReactFlow>
  );
}
