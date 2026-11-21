#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Checks relative links in Markdown files: README.md, CONTRIBUTING.md, SECURITY.md, LICENSING.md,
// docs/**, examples/**/*.md and the other *.md files under top-level directories (not node_modules).
// For every link `[text](target)` whose target is not an absolute URL or mailto:, the file (or
// directory) must exist, and a `#fragment` must match a heading of the target Markdown file
// (GitHub-style slugs). External http(s) links are not fetched.
//
//   node scripts/check-docs-links.mjs
import { readdirSync, readFileSync, statSync, existsSync } from "node:fs";
import { dirname, join, resolve, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const SKIP = new Set(["node_modules", ".git", "dist", ".turbo", ".claude"]);

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    if (SKIP.has(name)) continue;
    const p = join(dir, name);
    const st = statSync(p);
    if (st.isDirectory()) walk(p, out);
    else if (name.endsWith(".md")) out.push(p);
  }
  return out;
}

function slug(heading) {
  return heading
    .trim()
    .toLowerCase()
    .replace(/<[^>]+>/g, "")
    .replace(/[`*_~]/g, "")
    .replace(/[^\p{L}\p{N}\s-]/gu, "")
    .replace(/\s/g, "-");
}

const anchorCache = new Map();
function anchors(file) {
  if (anchorCache.has(file)) return anchorCache.get(file);
  const set = new Set();
  const counts = new Map();
  let inFence = false;
  for (const line of readFileSync(file, "utf8").split(/\r?\n/)) {
    if (/^\s*(```|~~~)/.test(line)) { inFence = !inFence; continue; }
    if (inFence) continue;
    const m = /^#{1,6}\s+(.*?)\s*#*\s*$/.exec(line);
    if (!m) continue;
    const base = slug(m[1]);
    const n = counts.get(base) ?? 0;
    counts.set(base, n + 1);
    set.add(n === 0 ? base : `${base}-${n}`);
  }
  anchorCache.set(file, set);
  return set;
}

function links(text) {
  const out = [];
  let inFence = false;
  text.split(/\r?\n/).forEach((line, i) => {
    if (/^\s*(```|~~~)/.test(line)) { inFence = !inFence; return; }
    if (inFence) return;
    const stripped = line.replace(/`[^`]*`/g, "");
    for (const m of stripped.matchAll(/\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g)) out.push({ target: m[1], line: i + 1 });
  });
  return out;
}

let bad = 0;
let checked = 0;
const files = walk(root);
for (const file of files) {
  for (const { target, line } of links(readFileSync(file, "utf8"))) {
    if (/^[a-z][a-z0-9+.-]*:/i.test(target)) continue; // http:, https:, mailto:, ...
    const [pathPart, frag] = target.split("#");
    const dest = pathPart === "" ? file : resolve(dirname(file), decodeURIComponent(pathPart));
    checked++;
    const where = `${relative(root, file).split(sep).join("/")}:${line}`;
    if (!existsSync(dest)) { console.error(`${where}: missing target ${target}`); bad++; continue; }
    if (frag && dest.endsWith(".md") && !anchors(dest).has(frag.toLowerCase())) {
      console.error(`${where}: no heading for #${frag} in ${relative(root, dest).split(sep).join("/")}`);
      bad++;
    }
  }
}
console.log(`${files.length} markdown files, ${checked} relative links checked, ${bad} problem(s)`);
process.exit(bad ? 1 : 0);
