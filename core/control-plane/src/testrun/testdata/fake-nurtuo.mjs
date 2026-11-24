#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Fake `nurtuo` for runner tests. Behaviour is chosen with `--set mode=<x>` (the child's environment is
// minimal by design, so an env var cannot select it). Prints canned `nurtuo flow run --json` shapes.
import { createHash } from "node:crypto";
import { readFileSync, readdirSync, writeFileSync } from "node:fs";

const argv = process.argv.slice(2);
if (argv[1] === "host-key") {
  if (argv[0] !== "flow" || !/^--port 22 --timeout \d+ms --json$/.test(argv.slice(3).join(" ")) || argv[2] !== "8.8.8.8" ||
      Object.keys(process.env).some((name) => name.startsWith("NURTUO_"))) process.exit(2);
  console.log(JSON.stringify({ keys: [{ type: "ssh-ed25519", fingerprintSha256: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }] }));
  process.exit(0);
}
const props = {};
const connFiles = [];
for (let i = 0; i < argv.length; i++) {
  if (argv[i] === "--set") { const kv = argv[++i]; const k = kv.indexOf("="); props[kv.slice(0, k)] = kv.slice(k + 1); }
  if (argv[i] === "--connection") connFiles.push(argv[++i]);
}
const flow = JSON.parse(readFileSync(argv[2], "utf8"));
if (props.pidfile) writeFileSync(props.pidfile, String(process.pid));
const stepOut = (s, over = {}) => ({ id: s.id, kind: s.kind, in: 1, out: 1, failed: 0, dropped: 0, ...over });
const allOk = flow.steps.map((s) => stepOut(s));
const secretEnv = Object.entries(process.env).filter(([k]) => k.startsWith("NURTUO_SECRET_"));
const doc = (id, bytes, p = {}) => ({ id, properties: p, payload: Buffer.from(bytes).toString("base64") });
const fail = (code, message, steps, exit = 1) => { process.stderr.write(JSON.stringify({ error: { code, message }, ...(steps ? { steps } : {}) }) + "\n"); process.exit(exit); };
const hang = () => setInterval(() => {}, 1000);

switch (props.mode ?? "echo") {
  case "echo": {
    const tmp = process.env.TMPDIR;
    const env = { ...process.env };
    for (const [key, value] of secretEnv) env[key] = createHash("sha256").update(value).digest("hex");
    const info = { argv, env, cwd: process.cwd(), files: readdirSync(tmp).sort() };
    process.stdout.write(JSON.stringify({ status: 200, documents: [doc("flow-run.out", JSON.stringify(info), { path: props.path ?? "" })], steps: allOk }) + "\n");
    break;
  }
  case "text": process.stdout.write(JSON.stringify({ status: 200, documents: [doc("flow-run.out", props.text ?? "hello", { filename: "a.txt" })], steps: allOk }) + "\n"); break;
  case "binary": process.stdout.write(JSON.stringify({ status: 200, documents: [doc("flow-run.out", Buffer.from([0xff, 0xfe, 0x00, 0x01]))], steps: allOk }) + "\n"); break;
  case "bigdocs": process.stdout.write(JSON.stringify({ status: 200, documents: Array.from({ length: 21 }, (_, i) => doc(`d${i}`, "x")), steps: allOk }) + "\n"); break;
  case "bigpayload": process.stdout.write(JSON.stringify({ status: 200, documents: [doc("big", Buffer.alloc(1024 * 1024 + 1, 97))], steps: allOk }) + "\n"); break;
  case "badjson": process.stdout.write("this is not json\n"); break;
  case "flood": { const chunk = "x".repeat(65536); const t = setInterval(() => process.stdout.write(chunk), 1); t.unref?.(); hang(); break; }
  case "sleep": hang(); break;
  case "ignoreterm": process.on("SIGTERM", () => {}); hang(); break;
  case "spool": writeFileSync(`${process.env.TMPDIR}/spool.bin`, Buffer.alloc(2 * 1024 * 1024, 1)); hang(); break;
  case "stop-default": {
    const i = flow.steps.findIndex((s) => s.kind === "stop");
    process.stdout.write(JSON.stringify({ status: 200, documents: [], steps: allOk.slice(0, i + 1) }) + "\n");
    break;
  }
  case "stop": {
    const i = flow.steps.findIndex((s) => s.kind === "stop");
    fail("stopped", "flow stopped with status 404: gone", flow.steps.slice(0, i + 1).map((s) => stepOut(s)), 3);
    break;
  }
  case "fail": {
    const i = flow.steps.findIndex((s) => s.kind === "connector");
    fail("execution", "1 document(s) failed: call: no such file (path /nope.txt)", flow.steps.slice(0, i + 1).map((s, j) => stepOut(s, j === i ? { out: 0, failed: 1 } : {})));
    break;
  }
  case "prepare": {
    const id = connFiles[0].split("/").pop().replace(".connection.json", "");
    fail("execution", `connection "${id}" (sftp): test failed: ssh connect: host key mismatch: got SHA256:aaa, want SHA256:bbb`);
    break;
  }
  case "timeoutmsg": fail("execution", "timed out after 1s", [stepOut(flow.steps[0])]); break;
  case "leak": fail("execution", `login failed for password ${secretEnv[0]?.[1]}`); break;
  case "usage": fail("validation", "bad flow", undefined, 2); break;
  default: process.exit(9);
}
