// SPDX-License-Identifier: Apache-2.0
// End-to-end tests: spawn the built CLI against an in-process fake control plane.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";
import { startFakeControlPlane, type FakeControlPlane } from "@nurtuo/client/testing";

const BIN = fileURLToPath(new URL("./index.js", import.meta.url));
const FLOW = fileURLToPath(new URL("../../examples/flows/get-employee.flow.json", import.meta.url));

interface Result { code: number | null; stdout: string; stderr: string }

function nurtuoctl(args: string[], opts: { env?: Record<string, string>; stdin?: string } = {}): Promise<Result> {
  return new Promise((resolve, reject) => {
    const env = { ...process.env, ...opts.env } as Record<string, string>;
    for (const k of ["NURTUO_SERVER", "NURTUO_TOKEN"]) if (!(opts.env && k in opts.env)) delete env[k];
    const child = spawn(process.execPath, [BIN, ...args], { env, stdio: ["pipe", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
    child.stdin.end(opts.stdin ?? "");
  });
}

let dir: string;
let cp: FakeControlPlane;
let env: Record<string, string>;

before(async () => {
  dir = mkdtempSync(join(tmpdir(), "nurtuoctl-"));
  cp = await startFakeControlPlane({ token: "tok" });
  env = { NURTUO_CONFIG: join(dir, "config.json") };
});
after(async () => {
  await cp.close();
  rmSync(dir, { recursive: true, force: true });
});

const writeFlow = (name: string, mutate?: (f: any) => void) => {
  const f = JSON.parse(readFileSync(FLOW, "utf8"));
  mutate?.(f);
  const p = join(dir, name);
  writeFileSync(p, JSON.stringify(f));
  return p;
};
const json = (r: Result) => JSON.parse(r.stdout);

test("help: root, per command, and unknown command", async () => {
  const root = await nurtuoctl([]);
  assert.equal(root.code, 0);
  for (const c of ["validate", "login", "component", "flow", "deploy", "runtime", "diff", "fmt", "init", "completion"]) assert.match(root.stdout, new RegExp(`\\n  ${c}\\s`));
  for (const c of ["validate", "login", "logout", "component list", "component get", "component create", "component delete", "component revisions", "flow push", "flow pull", "deploy", "deploy status", "runtime list", "diff", "fmt", "init", "completion"]) {
    const r = await nurtuoctl([...c.split(" "), "--help"]);
    assert.equal(r.code, 0, c);
    assert.match(r.stdout, new RegExp(`Usage: nurtuoctl ${c}`), c);
  }
  assert.equal((await nurtuoctl(["help", "flow", "push"])).code, 0);
  const bad = await nurtuoctl(["bogus"]);
  assert.equal(bad.code, 2);
  assert.match(bad.stderr, /unknown command "bogus"/);
  assert.equal((await nurtuoctl(["component"])).code, 2);
  assert.equal((await nurtuoctl(["component", "bogus"])).code, 2);
  assert.match((await nurtuoctl(["--version"])).stdout, /^\d+\.\d+\.\d+/);
});

test("validate: ok, errors, json, stdin, bad file", async () => {
  const ok = await nurtuoctl(["validate", FLOW]);
  assert.equal(ok.code, 0);
  const bad = writeFlow("bad.json", (f) => { f.edges["found:true"] = "ghost"; });
  const r = await nurtuoctl(["validate", bad]);
  assert.equal(r.code, 1);
  assert.match(r.stdout, /^error\tedge\./m);
  const j = await nurtuoctl(["validate", bad, "--json"]);
  assert.equal(j.code, 1);
  assert.equal(json(j).valid, false);
  assert.ok(json(j).diagnostics.length > 0);
  const stdin = await nurtuoctl(["validate", "-"], { stdin: readFileSync(FLOW, "utf8") });
  assert.equal(stdin.code, 0);
  const missing = await nurtuoctl(["validate", join(dir, "nope.json")]);
  assert.equal(missing.code, 1);
  assert.match(missing.stderr, /no such file/);
  writeFileSync(join(dir, "garbage.json"), "{nope");
  const garbage = await nurtuoctl(["validate", join(dir, "garbage.json"), "--json"]);
  assert.equal(garbage.code, 1);
  assert.equal(JSON.parse(garbage.stderr).error.code, "invalid_json");
  assert.equal((await nurtuoctl(["validate"])).code, 2);
});

test("commands without credentials fail with a usage error; login rejects a bad token and stores nothing", async () => {
  const none = await nurtuoctl(["component", "list"], { env });
  assert.equal(none.code, 2);
  assert.match(none.stderr, /no server configured/);

  const denied = await nurtuoctl(["login", "--server", cp.url, "--token", "wrong"], { env });
  assert.equal(denied.code, 4);
  assert.equal(existsSync(env.NURTUO_CONFIG!), false);

  const badUrl = await nurtuoctl(["login", "--server", "ftp://x"], { env });
  assert.equal(badUrl.code, 2);
  const noServer = await nurtuoctl(["login"], { env });
  assert.equal(noServer.code, 2);
});

test("login stores config (0600 where supported); token never printed; env and flags override", async () => {
  const r = await nurtuoctl(["login", "--server", cp.url + "/", "--token-stdin", "--json"], { env, stdin: "tok\n" });
  assert.equal(r.code, 0, r.stderr);
  assert.equal(json(r).verified, true);
  assert.ok(!r.stdout.includes("tok\""));
  const stored = JSON.parse(readFileSync(env.NURTUO_CONFIG!, "utf8"));
  assert.deepEqual(stored, { server: cp.url, token: "tok" });
  if (process.platform !== "win32") assert.equal(statSync(env.NURTUO_CONFIG!).mode & 0o777, 0o600);

  assert.equal((await nurtuoctl(["component", "list"], { env })).code, 0);
  // a wrong env token overrides the stored one
  assert.equal((await nurtuoctl(["component", "list"], { env: { ...env, NURTUO_TOKEN: "bad" } })).code, 4);
  // flag beats env
  assert.equal((await nurtuoctl(["component", "list", "--token", "tok"], { env: { ...env, NURTUO_TOKEN: "bad" } })).code, 0);
  // the authorization header reached the server
  assert.equal(cp.requests.at(-1)!.authorization, "Bearer tok");
});

test("component create/list/get/delete incl. error paths", async () => {
  const c = await nurtuoctl(["component", "create", "orders", "-d", "Order flow", "--json"], { env });
  assert.equal(c.code, 0, c.stderr);
  const comp = json(c);
  assert.equal(comp.name, "orders");
  assert.equal(comp.kind, "flow");

  const dup = await nurtuoctl(["component", "create", "orders"], { env });
  assert.equal(dup.code, 1);
  assert.match(dup.stderr, /already exists/);
  const badName = await nurtuoctl(["component", "create", "Bad Name", "--json"], { env });
  assert.equal(badName.code, 1);
  assert.equal(JSON.parse(badName.stderr).error.code, "bad_request");

  const list = await nurtuoctl(["component", "list"], { env });
  assert.match(list.stdout, /ID\s+NAME\s+KIND/);
  assert.match(list.stdout, /orders\s+flow\s+Order flow/);
  assert.equal(json(await nurtuoctl(["component", "list", "--json"], { env })).items.length, 1);

  assert.equal(json(await nurtuoctl(["component", "get", "orders", "--json"], { env })).id, comp.id);
  assert.equal(json(await nurtuoctl(["component", "get", comp.id, "--json"], { env })).name, "orders");
  const nf = await nurtuoctl(["component", "get", "ghost"], { env });
  assert.equal(nf.code, 1);
  assert.match(nf.stderr, /component "ghost" not found/);

  assert.equal((await nurtuoctl(["component", "delete", "orders"], { env })).code, 0);
  assert.equal(json(await nurtuoctl(["component", "list", "--json"], { env })).items.length, 0);
  assert.equal((await nurtuoctl(["component", "delete", "orders"], { env })).code, 1);
  assert.equal((await nurtuoctl(["component", "create", "x", "--kind", "bogus"], { env })).code, 1);
  assert.equal((await nurtuoctl(["component", "create"], { env })).code, 2);
  assert.equal((await nurtuoctl(["component", "create", "a", "--nope"], { env })).code, 2);
});

test("flow push / pull / deploy / status / runtime list end to end", async () => {
  // not found without --create, validation errors abort before any request
  const before = cp.requests.length;
  const noComp = await nurtuoctl(["flow", "push", FLOW], { env });
  assert.equal(noComp.code, 1);
  assert.match(noComp.stderr, /use --create/);
  const bad = writeFlow("bad-push.json", (f) => { f.steps.pop(); });
  const rejected = await nurtuoctl(["flow", "push", bad, "--create"], { env });
  assert.equal(rejected.code, 1);
  assert.match(rejected.stderr, /not pushed/);
  assert.equal(cp.requests.slice(before).filter((r) => r.method === "POST").length, 0);

  const p1 = await nurtuoctl(["flow", "push", FLOW, "--create", "--json"], { env });
  assert.equal(p1.code, 0, p1.stderr);
  assert.deepEqual([json(p1).component, json(p1).revision, json(p1).created], ["get-employee", 1, true]);

  // identical content is not pushed again unless forced; key order is irrelevant
  const reordered = join(dir, "reordered.json");
  const doc = JSON.parse(readFileSync(FLOW, "utf8"));
  writeFileSync(reordered, JSON.stringify({ edges: doc.edges, steps: doc.steps, revision: 1, name: doc.name, id: doc.id, schema: doc.schema }));
  const same = await nurtuoctl(["flow", "push", reordered, "--json"], { env });
  assert.equal(json(same).created, false);
  assert.equal(json(same).revision, 1);
  assert.equal(json(await nurtuoctl(["flow", "push", reordered, "--force", "--json"], { env })).revision, 2);

  const changed = writeFlow("changed.json", (f) => { f.steps[3].config.jsonata = "{ 'id': id }"; });
  const p3 = await nurtuoctl(["flow", "push", changed, "--json"], { env });
  assert.equal(json(p3).revision, 3);

  const revs = await nurtuoctl(["component", "revisions", "get-employee"], { env });
  assert.match(revs.stdout, /REV\s+ID\s+DIGEST/);

  // pull: latest to stdout, specific revision to a file
  const pulled = await nurtuoctl(["flow", "pull", "get-employee"], { env });
  assert.equal(pulled.code, 0);
  assert.equal(JSON.parse(pulled.stdout).steps[3].config.jsonata, "{ 'id': id }");
  assert.ok(pulled.stdout.endsWith("}\n"));
  const out = join(dir, "pulled.json");
  const toFile = await nurtuoctl(["flow", "pull", "get-employee", "-r", "1", "-o", out, "--json"], { env });
  assert.equal(toFile.code, 0);
  assert.equal(json(toFile).revision, 1);
  assert.equal(JSON.parse(readFileSync(out, "utf8")).steps[3].config.jsonata, doc.steps[3].config.jsonata);
  assert.equal((await nurtuoctl(["flow", "pull", "get-employee", "--revision", "9"], { env })).code, 1);
  assert.equal((await nurtuoctl(["flow", "pull", "get-employee", "--revision", "x"], { env })).code, 2);
  await nurtuoctl(["component", "create", "empty"], { env });
  assert.match((await nurtuoctl(["flow", "pull", "empty"], { env })).stderr, /no revisions/);

  // runtimes
  assert.match((await nurtuoctl(["runtime", "list"], { env })).stdout, /no runtimes/);
  cp.addRuntime({ id: "rt-dev", name: "dev runtime", environment: "dev", version: "0.1.0" });
  cp.addRuntime({ id: "rt-prod", name: "prod runtime", environment: "prod" });
  const rl = await nurtuoctl(["runtime", "list", "-e", "dev"], { env });
  assert.match(rl.stdout, /rt-dev\s+dev runtime\s+dev\s+online/);
  assert.doesNotMatch(rl.stdout, /rt-prod/);
  assert.equal(json(await nurtuoctl(["runtime", "list", "--json"], { env })).items.length, 2);

  // deploy: latest revision by default, explicit revision, errors
  const d1 = await nurtuoctl(["deploy", "get-employee", "--runtime", "rt-dev", "--json"], { env });
  assert.equal(d1.code, 0, d1.stderr);
  assert.equal(json(d1).revision, 3);
  assert.equal(json(d1).deployment.status, "pending");
  assert.equal(json(d1).generation, 1);
  const d2 = await nurtuoctl(["deploy", "get-employee", "--runtime", "rt-dev", "--revision", "1", "--json"], { env });
  assert.equal(json(d2).generation, 2);
  assert.match((await nurtuoctl(["deploy", "get-employee", "--runtime", "ghost"], { env })).stderr, /runtime not found/);
  assert.equal((await nurtuoctl(["deploy", "get-employee"], { env })).code, 2);
  assert.equal((await nurtuoctl(["deploy", "get-employee", "--runtime", "rt-dev", "--revision", "9"], { env })).code, 1);
  assert.match((await nurtuoctl(["deploy", "empty", "--runtime", "rt-dev"], { env })).stderr, /no revisions/);

  // status
  const id = json(d1).deployment.id as string;
  const st = await nurtuoctl(["deploy", "status", id], { env });
  assert.equal(st.code, 0);
  assert.match(st.stdout, new RegExp(`deployment ${id}\\s+pending`));
  assert.equal((await nurtuoctl(["deploy", "status", "dep_nope"], { env })).code, 1);
  assert.equal((await nurtuoctl(["deploy", "status"], { env })).code, 2);

  // --wait: runtime reports running -> exit 0; failed -> exit 1; never -> timeout
  const report = (status: string) => fetch(`${cp.url}/v1/deployments/${id}/status`, { method: "PUT", headers: { authorization: "Bearer tok", "content-type": "application/json" }, body: JSON.stringify({ status, message: "m" }) });
  const waiting = nurtuoctl(["deploy", "status", id, "--wait", "--interval", "20", "--json"], { env });
  setTimeout(() => void report("running"), 150);
  const w = await waiting;
  assert.equal(w.code, 0, w.stderr);
  assert.equal(json(w).deployment.status, "running");
  await report("failed");
  const failed = await nurtuoctl(["deploy", "status", id], { env });
  assert.equal(failed.code, 1);
  await report("pending");
  const to = await nurtuoctl(["deploy", "status", id, "--wait", "--timeout", "0", "--interval", "10"], { env });
  assert.equal(to.code, 1);
  assert.match(to.stderr, /timed out/);

  // deploy --wait
  const waitedDeploy = nurtuoctl(["deploy", "get-employee", "--runtime", "rt-prod", "--wait", "--interval", "20", "--json"], { env });
  // Poll until the CLI has created the deployment (a fixed delay races with process start-up on slow runners).
  const reported = (async () => {
    for (let i = 0; i < 500; i++) {
      const deps = (await (await fetch(`${cp.url}/v1/deployments?runtimeId=rt-prod`, { headers: { authorization: "Bearer tok" } })).json()) as { items: { id: string }[] };
      if (deps.items[0]) {
        await fetch(`${cp.url}/v1/deployments/${deps.items[0].id}/status`, { method: "PUT", headers: { authorization: "Bearer tok", "content-type": "application/json" }, body: JSON.stringify({ status: "running" }) });
        return;
      }
      await new Promise((r) => setTimeout(r, 20));
    }
    throw new Error("deployment for rt-prod never appeared");
  })();
  const wd = await waitedDeploy;
  await reported;
  assert.equal(wd.code, 0, wd.stderr);
  assert.equal(json(wd).deployment.status, "running");

  // delete refused while revisions exist -> 409 surfaced with exit 1
  const del = await nurtuoctl(["component", "delete", "get-employee"], { env });
  assert.equal(del.code, 1);
  assert.match(del.stderr, /has revisions \(HTTP 409\)/);
});

test("network failure exits 3; server 5xx exits 1", async () => {
  const r = await nurtuoctl(["component", "list", "--server", "http://127.0.0.1:1"], { env });
  assert.equal(r.code, 3);
  assert.match(r.stderr, /nurtuoctl:/);
  const j = await nurtuoctl(["component", "list", "--server", "http://127.0.0.1:1", "--json"], { env });
  assert.equal(JSON.parse(j.stderr).error.code, "network");
  cp.failNext(1, 500);
  // GET is retried, so a single injected 500 is absorbed
  assert.equal((await nurtuoctl(["component", "list"], { env })).code, 0);
  cp.failNext(5, 500);
  const down = await nurtuoctl(["component", "list"], { env });
  assert.equal(down.code, 1);
  assert.match(down.stderr, /HTTP 500/);
  cp.failNext(0, 500);
});

test("diff: semantic by step id and edge", async () => {
  const a = FLOW;
  const same = await nurtuoctl(["diff", a, a, "--exit-code"]);
  assert.equal(same.code, 0);
  assert.match(same.stdout, /semantically identical/);

  // reordering steps and keys is not a difference
  const doc = JSON.parse(readFileSync(FLOW, "utf8"));
  const shuffled = join(dir, "shuffled.json");
  writeFileSync(shuffled, JSON.stringify({ ...doc, steps: [...doc.steps].reverse().map((s: any) => ({ config: s.config, kind: s.kind, id: s.id })) }));
  assert.equal(json(await nurtuoctl(["diff", a, shuffled, "--json"])).identical, true);

  const b = writeFlow("b.json", (f) => {
    f.name = "Renamed";
    f.steps[3].config.jsonata = "{ 'id': id }";
    f.steps.push({ id: "audit", kind: "notify", config: { channel: "ops" } });
    f.steps = f.steps.filter((s: any) => s.id !== "not_found");
    f.edges["found:false"] = "return";
    delete f.edges["to_response:default"];
    f.edges["audit:default"] = "return";
  });
  const r = await nurtuoctl(["diff", a, b]);
  assert.equal(r.code, 0);
  assert.match(r.stdout, /~ name: "GetEmployee" -> "Renamed"/);
  assert.match(r.stdout, /- step not_found \(stop\)/);
  assert.match(r.stdout, /\+ step audit \(notify\)/);
  assert.match(r.stdout, /~ step to_response\n\s+config\.jsonata: .* -> "\{ 'id': id \}"/);
  assert.match(r.stdout, /- edge to_response:default -> return/);
  assert.match(r.stdout, /\+ edge audit:default -> return/);
  assert.match(r.stdout, /~ edge found:false: not_found -> return/);
  const j = json(await nurtuoctl(["diff", a, b, "--json"]));
  assert.equal(j.identical, false);
  assert.deepEqual(j.steps.added, [{ id: "audit", kind: "notify" }]);
  assert.equal((await nurtuoctl(["diff", a, b, "--exit-code"])).code, 1);
  assert.equal((await nurtuoctl(["diff", a])).code, 2);
  assert.equal((await nurtuoctl(["diff", a, join(dir, "nope.json")])).code, 1);
});

test("fmt: stdout, --check, -w, idempotent", async () => {
  const messy = join(dir, "messy.json");
  const doc = JSON.parse(readFileSync(FLOW, "utf8"));
  writeFileSync(messy, JSON.stringify({ edges: Object.fromEntries(Object.entries(doc.edges).reverse()), steps: doc.steps.map((s: any) => ({ config: s.config, id: s.id, kind: s.kind })), name: doc.name, id: doc.id, revision: 1, schema: doc.schema }));
  const out = await nurtuoctl(["fmt", messy]);
  assert.equal(out.code, 0);
  const parsed = JSON.parse(out.stdout);
  assert.deepEqual(Object.keys(parsed), ["schema", "id", "name", "revision", "steps", "edges"]);
  assert.deepEqual(Object.keys(parsed.steps[0]), ["id", "kind", "config"]);
  assert.deepEqual(Object.keys(parsed.edges), Object.keys(parsed.edges).slice().sort());
  assert.ok(out.stdout.endsWith("\n") && out.stdout.includes('\n  "id"'));
  assert.deepEqual(parsed.steps, doc.steps.map((s: any) => JSON.parse(JSON.stringify(s))));

  assert.equal((await nurtuoctl(["fmt", messy, "--check"])).code, 1);
  const w = await nurtuoctl(["fmt", messy, "-w", "--json"]);
  assert.equal(json(w).changed, true);
  assert.equal((await nurtuoctl(["fmt", messy, "--check"])).code, 0);
  assert.equal(json(await nurtuoctl(["fmt", messy, "-w", "--json"])).changed, false);
  assert.equal((await nurtuoctl(["fmt", "-", "-w"], { stdin: "{}" })).code, 2);
  const viaStdin = await nurtuoctl(["fmt", "-"], { stdin: '{"b":1,"id":"x"}' });
  assert.equal(JSON.parse(viaStdin.stdout).id, "x");
  writeFileSync(join(dir, "broken.json"), "[");
  assert.equal((await nurtuoctl(["fmt", join(dir, "broken.json")])).code, 1);
});

test("init scaffolds valid flows from every template; refuses overwrite", async () => {
  for (const t of ["minimal", "http-get"]) {
    const file = join(dir, `${t}.flow.json`);
    const r = await nurtuoctl(["init", "My Flow", "--template", t, "-o", file, "--json"]);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(json(r).id, "my-flow");
    const v = await nurtuoctl(["validate", file]);
    assert.equal(v.code, 0, v.stdout);
    assert.equal((await nurtuoctl(["fmt", file, "--check"])).code, 0, "scaffold is canonically formatted");
    const again = await nurtuoctl(["init", "My Flow", "-t", t, "-o", file]);
    assert.equal(again.code, 1);
    assert.match(again.stderr, /already exists/);
    assert.equal((await nurtuoctl(["init", "My Flow", "-t", t, "-o", file, "--force"])).code, 0);
  }
  assert.equal((await nurtuoctl(["init", "x", "--template", "nope", "-o", join(dir, "x.json")])).code, 2);
  assert.equal((await nurtuoctl(["init", "!!!", "-o", join(dir, "y.json")])).code, 2);
  assert.equal((await nurtuoctl(["init"])).code, 2);
});

test("completion scripts for bash, zsh, powershell", async () => {
  const bash = await nurtuoctl(["completion", "bash"]);
  assert.equal(bash.code, 0);
  assert.match(bash.stdout, /complete -o default -F _nurtuoctl nurtuoctl/);
  assert.ok(bash.stdout.includes('"component") opts="list get create delete revisions'));
  assert.ok(bash.stdout.includes('"deploy") opts="status --json'));
  assert.match(bash.stdout, /"deploy"\) opts="[^"]*--runtime/);
  const zsh = await nurtuoctl(["completion", "zsh"]);
  assert.match(zsh.stdout, /^#compdef nurtuoctl/);
  assert.match(zsh.stdout, /compdef _nurtuoctl nurtuoctl/);
  const ps = await nurtuoctl(["completion", "powershell"]);
  assert.match(ps.stdout, /Register-ArgumentCompleter -Native -CommandName nurtuoctl/);
  assert.match(ps.stdout, /'flow' = 'push pull/);
  const bad = await nurtuoctl(["completion", "fish"]);
  assert.equal(bad.code, 2);
  assert.equal((await nurtuoctl(["completion"])).code, 2);
});

test("logout removes the config", async () => {
  assert.ok(existsSync(env.NURTUO_CONFIG!));
  const r = await nurtuoctl(["logout", "--json"], { env });
  assert.equal(json(r).removed, true);
  assert.equal(existsSync(env.NURTUO_CONFIG!), false);
  assert.equal(json(await nurtuoctl(["logout", "--json"], { env })).removed, false);
});
