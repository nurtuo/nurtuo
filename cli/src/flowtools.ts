// SPDX-License-Identifier: Apache-2.0
// Pure helpers over flow JSON: canonical formatting, semantic diff, templates.
import { FLOW_SCHEMA_VERSION } from "@nurtuo/flow-schema";

type Json = null | boolean | number | string | Json[] | { [k: string]: Json };
type Obj = Record<string, unknown>;

const isObj = (v: unknown): v is Obj => !!v && typeof v === "object" && !Array.isArray(v);

/** Recursively sorts object keys (arrays keep their order). */
export function sortKeys(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sortKeys);
  if (isObj(v)) return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sortKeys(v[k])]));
  return v;
}

/** Canonical JSON (sorted keys, compact). Used to compare documents regardless of key order. */
export function canonicalJson(v: unknown): string {
  return JSON.stringify(sortKeys(v));
}

const TOP_ORDER = ["schema", "id", "name", "description", "revision", "steps", "edges"];
const STEP_ORDER = ["id", "kind", "label", "config"];

function ordered(o: Obj, order: string[]): Obj {
  const out: Obj = {};
  for (const k of order) if (k in o) out[k] = o[k];
  for (const k of Object.keys(o).sort()) if (!(k in out)) out[k] = o[k];
  return out;
}

/**
 * Canonical flow formatting: fixed top-level and step key order, step config keys sorted, edges
 * sorted by key, steps keep their authored order, 2-space indent, trailing newline.
 */
export function formatFlow(flow: unknown): string {
  if (!isObj(flow)) return JSON.stringify(flow, null, 2) + "\n";
  const top = ordered(flow, TOP_ORDER);
  if (Array.isArray(top["steps"])) {
    top["steps"] = top["steps"].map((s) => {
      if (!isObj(s)) return s;
      const step = ordered(s, STEP_ORDER);
      if ("config" in step) step["config"] = sortKeys(step["config"]);
      return step;
    });
  }
  if (isObj(top["edges"])) {
    const e = top["edges"];
    top["edges"] = Object.fromEntries(Object.keys(e).sort().map((k) => [k, e[k]]));
  }
  return JSON.stringify(top, null, 2) + "\n";
}

// ---------------------------------------------------------------------------------------------
// Semantic diff
// ---------------------------------------------------------------------------------------------

export interface Change { path: string; from?: unknown; to?: unknown }
export interface FlowDiff {
  identical: boolean;
  meta: Change[];
  steps: {
    added: { id: string; kind?: unknown }[];
    removed: { id: string; kind?: unknown }[];
    changed: { id: string; changes: Change[] }[];
  };
  edges: {
    added: Record<string, string>;
    removed: Record<string, string>;
    changed: { key: string; from: string; to: string }[];
  };
}

function deepDiff(a: unknown, b: unknown, path: string, out: Change[]): void {
  if (isObj(a) && isObj(b)) {
    for (const k of [...new Set([...Object.keys(a), ...Object.keys(b)])].sort()) {
      const p = path ? `${path}.${k}` : k;
      if (!(k in a)) out.push({ path: p, to: b[k] });
      else if (!(k in b)) out.push({ path: p, from: a[k] });
      else deepDiff(a[k], b[k], p, out);
    }
  } else if (Array.isArray(a) && Array.isArray(b) && a.length === b.length) {
    a.forEach((x, i) => deepDiff(x, b[i], `${path}[${i}]`, out));
  } else if (canonicalJson(a) !== canonicalJson(b)) out.push({ path, from: a, to: b });
}

/** Compares two flows by step id and edge key; step order and key order are ignored. */
export function diffFlows(a: unknown, b: unknown): FlowDiff {
  const fa = isObj(a) ? a : {};
  const fb = isObj(b) ? b : {};
  const diff: FlowDiff = { identical: true, meta: [], steps: { added: [], removed: [], changed: [] }, edges: { added: {}, removed: {}, changed: [] } };

  const metaA = Object.fromEntries(Object.entries(fa).filter(([k]) => k !== "steps" && k !== "edges"));
  const metaB = Object.fromEntries(Object.entries(fb).filter(([k]) => k !== "steps" && k !== "edges"));
  deepDiff(metaA, metaB, "", diff.meta);

  const index = (f: Obj) => {
    const m = new Map<string, Obj>();
    if (Array.isArray(f["steps"])) for (const s of f["steps"]) if (isObj(s) && typeof s["id"] === "string") m.set(s["id"], s);
    return m;
  };
  const sa = index(fa);
  const sb = index(fb);
  for (const [id, s] of sa) if (!sb.has(id)) diff.steps.removed.push({ id, kind: s["kind"] });
  for (const [id, s] of sb) {
    const old = sa.get(id);
    if (!old) diff.steps.added.push({ id, kind: s["kind"] });
    else {
      const changes: Change[] = [];
      deepDiff(old, s, "", changes);
      if (changes.length) diff.steps.changed.push({ id, changes });
    }
  }

  const ea = isObj(fa["edges"]) ? fa["edges"] : {};
  const eb = isObj(fb["edges"]) ? fb["edges"] : {};
  for (const k of Object.keys(ea).sort()) if (!(k in eb)) diff.edges.removed[k] = String(ea[k]);
  for (const k of Object.keys(eb).sort()) {
    if (!(k in ea)) diff.edges.added[k] = String(eb[k]);
    else if (ea[k] !== eb[k]) diff.edges.changed.push({ key: k, from: String(ea[k]), to: String(eb[k]) });
  }

  diff.steps.added.sort((x, y) => x.id.localeCompare(y.id));
  diff.steps.removed.sort((x, y) => x.id.localeCompare(y.id));
  diff.steps.changed.sort((x, y) => x.id.localeCompare(y.id));
  diff.identical =
    !diff.meta.length && !diff.steps.added.length && !diff.steps.removed.length && !diff.steps.changed.length &&
    !Object.keys(diff.edges.added).length && !Object.keys(diff.edges.removed).length && !diff.edges.changed.length;
  return diff;
}

const show = (v: unknown) => (v === undefined ? "(none)" : JSON.stringify(v));

export function renderDiff(d: FlowDiff): string {
  if (d.identical) return "flows are semantically identical\n";
  const L: string[] = [];
  for (const c of d.meta) L.push(`~ ${c.path}: ${show(c.from)} -> ${show(c.to)}`);
  for (const s of d.steps.removed) L.push(`- step ${s.id} (${String(s.kind)})`);
  for (const s of d.steps.added) L.push(`+ step ${s.id} (${String(s.kind)})`);
  for (const s of d.steps.changed) {
    L.push(`~ step ${s.id}`);
    for (const c of s.changes) L.push(`    ${c.path}: ${show(c.from)} -> ${show(c.to)}`);
  }
  for (const [k, v] of Object.entries(d.edges.removed)) L.push(`- edge ${k} -> ${v}`);
  for (const [k, v] of Object.entries(d.edges.added)) L.push(`+ edge ${k} -> ${v}`);
  for (const e of d.edges.changed) L.push(`~ edge ${e.key}: ${e.from} -> ${e.to}`);
  const n = d.steps.added.length + d.steps.removed.length + d.steps.changed.length + Object.keys(d.edges.added).length + Object.keys(d.edges.removed).length + d.edges.changed.length + d.meta.length;
  L.push(`${n} difference${n === 1 ? "" : "s"}`);
  return L.join("\n") + "\n";
}

// ---------------------------------------------------------------------------------------------
// Templates for `nurtuo init`
// ---------------------------------------------------------------------------------------------

export const TEMPLATES = {
  minimal: "Manual start that maps a document and returns it",
  "http-get": "HTTP listener, connector lookup, found/not-found decision",
} as const;
export type TemplateName = keyof typeof TEMPLATES;

/** Component-name-safe slug (also a valid flow id). */
export function slugify(name: string): string {
  return name.trim().toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+|-+$/g, "");
}

export function scaffold(name: string, template: TemplateName): Json {
  const id = slugify(name);
  const base = { schema: FLOW_SCHEMA_VERSION, id, name, revision: 1 };
  if (template === "http-get") {
    return {
      ...base,
      steps: [
        { id: "start", kind: "start", config: { type: "listener", connector: "http", method: "GET", path: `/${id}/{id}` } },
        { id: "lookup", kind: "connector", config: { connector: "postgres", operation: "QUERY", objectType: "record" } },
        { id: "found", kind: "decision", config: { cel: "doc.id != ''" } },
        { id: "shape", kind: "map", config: { jsonata: "$" } },
        { id: "return", kind: "returnDocuments", config: {} },
        { id: "not_found", kind: "stop", config: { status: 404, message: "not found" } },
      ],
      edges: {
        "start:default": "lookup",
        "lookup:default": "found",
        "found:true": "shape",
        "found:false": "not_found",
        "shape:default": "return",
      },
    };
  }
  return {
    ...base,
    steps: [
      { id: "start", kind: "start", config: { type: "manual" } },
      { id: "shape", kind: "map", config: { jsonata: "$" } },
      { id: "return", kind: "returnDocuments", config: {} },
    ],
    edges: { "start:default": "shape", "shape:default": "return" },
  };
}
