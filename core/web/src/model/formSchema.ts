// SPDX-License-Identifier: AGPL-3.0-only
// Minimal JSON Schema -> form description. Strings, numbers, booleans and enums get native
// controls; arrays/objects (and anything else) are edited as JSON text.
import type { Schema } from "./catalog";

export type FieldType = "string" | "number" | "integer" | "boolean" | "enum" | "json";

export interface Field {
  name: string;
  type: FieldType;
  required: boolean;
  options?: unknown[];
  description?: string;
  min?: number;
  max?: number;
  /** Secret input: rendered as type=password, never pre-filled. */
  writeOnly?: boolean;
}

export function fieldsOf(schema: Schema): Field[] {
  const required = new Set<string>(Array.isArray(schema["required"]) ? schema["required"] : []);
  const props = (schema["properties"] ?? {}) as Record<string, Schema>;
  return Object.entries(props).map(([name, p]) => {
    let type: FieldType = "json";
    if (Array.isArray(p["enum"])) type = "enum";
    else if (p["type"] === "string") type = "string";
    else if (p["type"] === "number") type = "number";
    else if (p["type"] === "integer") type = "integer";
    else if (p["type"] === "boolean") type = "boolean";
    return {
      name,
      type,
      required: required.has(name),
      ...(type === "enum" ? { options: p["enum"] as unknown[] } : {}),
      ...(typeof p["description"] === "string" ? { description: p["description"] } : {}),
      ...(typeof p["minimum"] === "number" ? { min: p["minimum"] } : {}),
      ...(typeof p["maximum"] === "number" ? { max: p["maximum"] } : {}),
      ...(p["writeOnly"] === true ? { writeOnly: true } : {}),
    };
  });
}

/** Text shown in the editor for a stored value. */
export function toText(f: Field, v: unknown): string {
  if (v === undefined) return "";
  return f.type === "json" ? JSON.stringify(v, null, 2) : String(v);
}

export type Parsed = { ok: true; value: unknown } | { ok: false; error: string };

/** Parses editor text into a value; `value: undefined` means "unset" (optional empty field). */
export function fromText(f: Field, text: string): Parsed {
  if (text.trim() === "" && f.type !== "string") {
    return f.required ? { ok: false, error: "Required" } : { ok: true, value: undefined };
  }
  switch (f.type) {
    case "string":
      return text === "" && !f.required ? { ok: true, value: undefined } : { ok: true, value: text };
    case "number":
    case "integer": {
      const n = Number(text);
      if (!Number.isFinite(n)) return { ok: false, error: "Not a number" };
      if (f.type === "integer" && !Number.isInteger(n)) return { ok: false, error: "Not an integer" };
      return { ok: true, value: n };
    }
    case "boolean":
      return { ok: true, value: text === "true" };
    case "enum":
      return { ok: true, value: f.options?.find((o) => String(o) === text) ?? text };
    default:
      try {
        return { ok: true, value: JSON.parse(text) as unknown };
      } catch (e) {
        return { ok: false, error: `Invalid JSON: ${(e as Error).message}` };
      }
  }
}

/** Returns a copy of `config` with `name` set (or removed when `value` is undefined). */
export function withField(config: Record<string, unknown>, name: string, value: unknown): Record<string, unknown> {
  const next = { ...config };
  if (value === undefined) delete next[name];
  else next[name] = value;
  return next;
}
