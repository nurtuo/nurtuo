#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Fails when an example flow calls a connector operation that the connector's catalog
// (connectors/<id>/connector.yaml) does not declare. Regression guard for e.g. postgres `GET`.
import { readFileSync, readdirSync, existsSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const ops = {}; // connector id -> Set of operation names (type, or customType for EXECUTE)
for (const d of readdirSync(join(root, "connectors"), { withFileTypes: true })) {
  const f = join(root, "connectors", d.name, "connector.yaml");
  if (!d.isDirectory() || !existsSync(f)) continue;
  const text = readFileSync(f, "utf8");
  const id = /^id:\s*(\S+)/m.exec(text)?.[1];
  if (!id) continue;
  const set = new Set();
  for (const line of text.split(/\r?\n/)) {
    if (!/^\s*-\s*[{t]/.test(line) && !/^\s*-\s*type:/.test(line)) continue;
    const ct = /customType:\s*([A-Z_]+)/.exec(line)?.[1];
    const t = /type:\s*([A-Z_]+)/.exec(line)?.[1];
    if (ct) set.add(ct); else if (t) set.add(t);
  }
  ops[id] = set;
}

let bad = 0;
const dir = join(root, "examples", "flows");
for (const f of readdirSync(dir).filter((n) => n.endsWith(".flow.json"))) {
  const flow = JSON.parse(readFileSync(join(dir, f), "utf8"));
  for (const s of flow.steps ?? []) {
    if (s.kind !== "connector") continue;
    const { connector, operation } = s.config ?? {};
    const known = ops[connector];
    if (!known) { console.error(`${f}: step ${s.id}: unknown connector '${connector}'`); bad++; continue; }
    if (!known.has(String(operation).toUpperCase())) {
      console.error(`${f}: step ${s.id}: connector '${connector}' has no operation '${operation}' (has: ${[...known].join(", ")})`);
      bad++;
    }
  }
}
if (bad) process.exit(1);
console.log("example flows use only declared connector operations");
