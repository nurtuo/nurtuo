// SPDX-License-Identifier: AGPL-3.0-only
// Guardrails shared by every tool: size limits, per-call timeout, secret redaction in outputs.
import { ToolError } from "./errors.js";

export interface Limits {
  /** Max ops in one apply_ops call. */
  maxOps: number;
  /** Max serialized size of tool arguments, bytes. */
  maxInputBytes: number;
  /** Max serialized size of a tool result, bytes. */
  maxOutputBytes: number;
  /** Per-call deadline, ms. */
  callTimeoutMs: number;
  /** Max steps a flow may have after apply_ops. */
  maxSteps: number;
}

export const DEFAULT_LIMITS: Limits = {
  maxOps: 200,
  maxInputBytes: 256 * 1024,
  maxOutputBytes: 512 * 1024,
  callTimeoutMs: 30_000,
  maxSteps: 500,
};

export const REDACTED = "[REDACTED]";

/** Object keys whose values are never returned to the model. */
const SECRET_KEY =
  /(^|_)(pass|password|passwd|passphrase|secret|token|api_?key|authorization|private_?key|credentials?|creds|nkey_?seed|access_?key|key_?pem|cookie)(_|$)/;
/** Keys that describe a secret (its name, type, location) rather than hold it. */
const DESCRIPTOR_SUFFIX = /_(name|in|type|ref|id|path|policy|field|header|count|expires?(_at|_in)?)$/;

/** Values that look like secrets regardless of the key they sit under. */
const SECRET_VALUE: [RegExp, string][] = [
  [/-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)/g, REDACTED],
  [/\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}/gi, `$1 ${REDACTED}`],
  [/(:\/\/[^\s/:@]+:)[^\s/@]+@/g, `$1${REDACTED}@`],
  [/\b(sk|pk|ghp|gho|xox[abp])[-_][A-Za-z0-9_-]{16,}/g, REDACTED],
  [/\bAKIA[0-9A-Z]{16}\b/g, REDACTED],
];

export function isSecretKey(key: string): boolean {
  const k = key.replace(/([a-z0-9])([A-Z])/g, "$1_$2").replace(/[\-.\s]+/g, "_").toLowerCase();
  return SECRET_KEY.test(k) && !DESCRIPTOR_SUFFIX.test(k);
}

/** Pure, deep redaction of secrets from JSON-like data. Never mutates the input. */
export function redact<T>(value: T): T {
  return walk(value, 0) as T;
}

function walk(v: unknown, depth: number): unknown {
  if (depth > 64) return REDACTED;
  if (typeof v === "string") return SECRET_VALUE.reduce((s, [re, rep]) => s.replace(re, rep), v);
  if (Array.isArray(v)) return v.map((x) => walk(x, depth + 1));
  if (v && typeof v === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v as Record<string, unknown>)) {
      // Keep empty/placeholder values visible: the agent needs to see a field is unset or a reference.
      out[k] = isSecretKey(k) && isLiteralSecret(x) ? REDACTED : walk(x, depth + 1);
    }
    return out;
  }
  return v;
}

/** `${secret:name}` / `{{secret.name}}` style references are not secrets; empty values neither. */
function isLiteralSecret(v: unknown): boolean {
  if (v === null || v === undefined || v === "" || typeof v === "boolean") return false;
  if (typeof v === "string") return !/^\s*(\$\{[^}]+\}|\{\{[^}]+\}\}|secret:\/\/\S+|\$[A-Z_][A-Z0-9_]*)\s*$/.test(v);
  return true;
}

/** Paths holding literal secrets (secret-looking key with a literal value, or a secret-looking
 *  string value). apply_ops refuses such input: secrets must never be written into flows. */
export function findLiteralSecrets(data: unknown, base = ""): string[] {
  const found: string[] = [];
  const rec = (v: unknown, p: string, d: number) => {
    if (d > 64) return;
    if (typeof v === "string") {
      if (SECRET_VALUE.reduce((s, [re, rep]) => s.replace(re, rep), v) !== v) found.push(p || "/");
      return;
    }
    if (!v || typeof v !== "object") return;
    for (const [k, x] of Object.entries(v as Record<string, unknown>)) {
      const path = `${p}/${k}`;
      if (isSecretKey(k) && isLiteralSecret(x)) found.push(path);
      else rec(x, path, d + 1);
    }
  };
  rec(data, base, 0);
  return found;
}

export const byteSize = (v: unknown): number => Buffer.byteLength(JSON.stringify(v) ?? "", "utf8");

export function checkInputSize(args: unknown, limits: Limits): void {
  const n = byteSize(args);
  if (n > limits.maxInputBytes) {
    throw new ToolError("limit_exceeded", `Input is ${n} bytes; the limit is ${limits.maxInputBytes}. Split the work into smaller calls.`, {
      limit: "maxInputBytes", max: limits.maxInputBytes, actual: n,
    });
  }
}

export async function withTimeout<T>(work: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  const deadline = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new ToolError("timeout", `${what} did not finish within ${ms} ms`, { limit: "callTimeoutMs", ms })), ms);
  });
  try {
    return await Promise.race([work, deadline]);
  } finally {
    clearTimeout(timer);
  }
}
