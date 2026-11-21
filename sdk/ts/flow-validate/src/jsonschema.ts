// SPDX-License-Identifier: Apache-2.0
// A deliberately small interpreter for the subset of JSON Schema 2020-12 that flow.v1 uses.
// It exists (instead of Ajv) so that the TS and Go validators produce *identical* diagnostics
// (same codes and JSON Pointer paths) from the same schema file, and so the validator has no
// runtime dependencies and runs in the browser. schema.test.ts cross-checks it against Ajv.
//
// Supported keywords: $ref (#/$defs/..), type, const, enum, minLength, pattern, minimum,
// maximum, minItems, items, required, properties, additionalProperties, propertyNames, allOf,
// if/then. Everything else (title, description, $id, $defs, ...) is ignored. A failing `type`
// stops evaluation of the remaining keywords of that schema node.

export interface RawIssue {
  code: string; // "schema.<keyword>"
  path: string; // JSON Pointer ("" is the document root)
  message: string;
}

type Schema = Record<string, any>; // eslint-disable-line @typescript-eslint/no-explicit-any

export function escapePointer(token: string): string {
  return token.replace(/~/g, "~0").replace(/\//g, "~1");
}

function typeOf(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  if (typeof v === "number") return Number.isInteger(v) ? "integer" : "number";
  return typeof v; // string | boolean | object
}

function typeMatches(want: string, v: unknown): boolean {
  const t = typeOf(v);
  return want === t || (want === "number" && t === "integer");
}

export function checkSchema(root: Schema, value: unknown): RawIssue[] {
  const out: RawIssue[] = [];
  const resolve = (ref: string): Schema => {
    if (!ref.startsWith("#/")) throw new Error(`unsupported $ref ${ref}`);
    let cur: any = root; // eslint-disable-line @typescript-eslint/no-explicit-any
    for (const part of ref.slice(2).split("/")) cur = cur?.[part];
    if (cur === undefined) throw new Error(`unresolved $ref ${ref}`);
    return cur;
  };

  const check = (schema: Schema, v: unknown, path: string, sink: RawIssue[]): void => {
    const add = (code: string, message: string, at = path) => sink.push({ code: `schema.${code}`, path: at, message });

    if (typeof schema.$ref === "string") check(resolve(schema.$ref), v, path, sink);

    if (schema.type !== undefined && !typeMatches(schema.type, v)) {
      add("type", `expected ${schema.type}, got ${typeOf(v)}`);
      return;
    }
    if ("const" in schema && v !== schema.const) add("const", `must equal ${JSON.stringify(schema.const)}`);
    if (Array.isArray(schema.enum) && !schema.enum.includes(v)) add("enum", `must be one of ${schema.enum.join(", ")}`);

    if (typeof v === "string") {
      if (typeof schema.minLength === "number" && [...v].length < schema.minLength) add("minLength", `shorter than ${schema.minLength}`);
      if (typeof schema.pattern === "string" && !new RegExp(schema.pattern).test(v)) add("pattern", `does not match ${schema.pattern}`);
    }
    if (typeof v === "number") {
      if (typeof schema.minimum === "number" && v < schema.minimum) add("minimum", `below ${schema.minimum}`);
      if (typeof schema.maximum === "number" && v > schema.maximum) add("maximum", `above ${schema.maximum}`);
    }
    if (Array.isArray(v)) {
      if (typeof schema.minItems === "number" && v.length < schema.minItems) add("minItems", `fewer than ${schema.minItems} items`);
      if (schema.items) v.forEach((item, i) => check(schema.items, item, `${path}/${i}`, sink));
    }
    if (typeof v === "object" && v !== null && !Array.isArray(v)) {
      const obj = v as Record<string, unknown>;
      const props: Record<string, Schema> = schema.properties ?? {};
      for (const r of schema.required ?? []) {
        if (!Object.prototype.hasOwnProperty.call(obj, r)) add("required", `missing required property ${r}`, `${path}/${escapePointer(r)}`);
      }
      for (const [k, sub] of Object.entries(props)) {
        if (Object.prototype.hasOwnProperty.call(obj, k)) check(sub, obj[k], `${path}/${escapePointer(k)}`, sink);
      }
      for (const k of Object.keys(obj)) {
        const at = `${path}/${escapePointer(k)}`;
        if (schema.propertyNames) check(schema.propertyNames, k, at, sink);
        if (Object.prototype.hasOwnProperty.call(props, k)) continue;
        if (schema.additionalProperties === false) add("additionalProperties", `unknown property ${k}`, at);
        else if (typeof schema.additionalProperties === "object") check(schema.additionalProperties, obj[k], at, sink);
      }
    }
    for (const sub of schema.allOf ?? []) check(sub, v, path, sink);
    if (schema.if) {
      const probe: RawIssue[] = [];
      check(schema.if, v, path, probe);
      if (probe.length === 0 && schema.then) check(schema.then, v, path, sink);
    }
  };

  check(root, value, "", out);
  return out;
}
