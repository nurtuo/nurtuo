// SPDX-License-Identifier: AGPL-3.0-only
// TestRunner: runs an inline flow through `nurtuo flow run --json` as a short-lived child process
// (canvas-run spec D7). One process per run, a per-run temp dir, a from-scratch child environment.
import { spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import { access, constants, lstat, mkdtemp, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { hasErrors, validateFlow } from "@nurtuo/flow-validate";
import { ApiError } from "../domain.js";
import type { ConnectionRepository } from "../connections/repository.js";
import { envVar } from "../connections/env-var.js";
import type { SecretStore } from "../connections/secrets.js";
import { secretRefs } from "../connections/validate.js";
import { checkEgress, type Lookup } from "./egress.js";
import { checkFlowPolicy, type FlowStep } from "./policy.js";
import type { RunLimiter } from "./semaphore.js";

export const DEFAULT_TIMEOUT_MS = 30_000;
export const MIN_TIMEOUT_MS = 1_000;
export const MAX_TIMEOUT_MS = 120_000;
const PROP_KEY = /^[A-Za-z_][A-Za-z0-9_.-]{0,63}$/;

export interface TestRunDeps {
  connections: ConnectionRepository;
  secrets: SecretStore;
  limiter: RunLimiter;
  /** Absolute path of the nurtuo binary (NURTUO_FLOW_RUN_BIN). */
  bin?: string;
  allowPrivate?: boolean;
  lookup?: Lookup;
  platform?: NodeJS.Platform;
  tmpRoot?: string;
  /** Test knobs; defaults are the spec's limits. */
  killGraceMs?: number;
  deadlineSlackMs?: number;
  spoolPollMs?: number;
  spoolLimitBytes?: number;
  stdoutLimit?: number;
  stderrLimit?: number;
  maxDocuments?: number;
  maxPayloadBytes?: number;
}

export interface TestRunRequest { flow?: unknown; input?: { properties?: unknown }; timeoutMs?: unknown }
export interface RunOutcome { status: number; body: unknown; audit?: { after: { runId: string; status: string } } }

type StepStatus = "ok" | "error" | "stopped" | "not_run";
interface CliStep { id: string; kind: string; in: number; out: number; failed: number; dropped: number }
interface CliError { code: string; message: string }

/** Replaces every secret value in text; defence in depth on top of the child's own Redactor. */
export function redact(text: string, values: readonly string[]): string {
  let out = text;
  for (const v of values) if (v.length >= 3) out = out.split(v).join("[redacted]");
  return out;
}

async function dirSize(dir: string): Promise<number> {
  let total = 0;
  for (const e of await readdir(dir, { withFileTypes: true }).catch(() => [])) {
    const p = join(dir, e.name);
    if (e.isDirectory()) total += await dirSize(p);
    else total += (await lstat(p).catch(() => undefined))?.size ?? 0;
  }
  return total;
}

export class TestRunner {
  constructor(private d: TestRunDeps) {}

  async available(): Promise<boolean> {
    if ((this.d.platform ?? process.platform) === "win32" || !this.d.bin) return false;
    try { await access(this.d.bin, constants.X_OK); return true; } catch { return false; }
  }

  async run(workspaceId: string, req: TestRunRequest, signal?: AbortSignal): Promise<RunOutcome> {
    const d = this.d;
    if (!(await this.available())) throw new ApiError(503, "test_run_unavailable", "test runs are not configured on this server");

    // 1. limits
    const timeoutMs = req.timeoutMs === undefined ? DEFAULT_TIMEOUT_MS : req.timeoutMs;
    if (typeof timeoutMs !== "number" || !Number.isInteger(timeoutMs) || timeoutMs < MIN_TIMEOUT_MS || timeoutMs > MAX_TIMEOUT_MS) throw new ApiError(400, "bad_request", `timeoutMs must be an integer between ${MIN_TIMEOUT_MS} and ${MAX_TIMEOUT_MS}`);
    const props = this.inputProperties(req.input);

    // 2. flow validation and D11 policy
    const diags = validateFlow(req.flow);
    if (hasErrors(diags)) return { status: 400, body: { error: { code: "invalid_flow", message: "the flow is not valid", diagnostics: diags } } };
    const flow = req.flow as { steps: FlowStep[] };
    const uses = checkFlowPolicy(flow.steps);

    const release = d.limiter.tryAcquire(workspaceId);
    if (!release) throw new ApiError(429, "too_many_runs", "another run is in progress, retry shortly");
    let tmp: string | undefined;
    try {
      // 3. connections and secrets of this workspace only
      const ids = [...new Set(uses.map((u) => u.connection))];
      const conns = [];
      const env: Record<string, string> = {};
      const secretValues: string[] = [];
      for (const id of ids) {
        const c = await d.connections.get(workspaceId, id);
        if (!c) throw new ApiError(404, "connection_not_found", `connection "${id}" not found`);
        for (const r of secretRefs(c.connector, c.settings)) {
          const v = await d.secrets.reveal(workspaceId, r.name);
          if (v === undefined) throw new ApiError(409, "secret_missing", `secret "${r.name}" of connection "${id}" is not set; open Connections and enter it again`);
          env[envVar(r.name)] = v;
          secretValues.push(v);
        }
        conns.push(c);
      }
      // 4. egress
      for (const c of conns) await checkEgress(String(c.settings.host ?? ""), { allowPrivate: d.allowPrivate ?? false, lookup: d.lookup });

      // 5. temp dir with the flow and non-secret connection files
      tmp = await mkdtemp(join(d.tmpRoot ?? tmpdir(), "nurtuo-testrun-"));
      const flowFile = join(tmp, "flow.json");
      await writeFile(flowFile, JSON.stringify(req.flow), { mode: 0o600 });
      const argv = ["flow", "run", flowFile, "--json", "--timeout", `${timeoutMs}ms`];
      for (const c of conns) {
        const f = join(tmp, `${c.id}.connection.json`);
        await writeFile(f, JSON.stringify({ schema: "nurtuo.connection/v1", id: c.id, connector: c.connector, settings: c.settings }), { mode: 0o600 });
        argv.push("--connection", f);
      }
      for (const [k, v] of Object.entries(props)) argv.push("--set", `${k}=${v}`);

      // 6-7. spawn and supervise
      const runId = randomUUID();
      const started = Date.now();
      const childEnv: Record<string, string> = { PATH: process.env.PATH ?? "/usr/local/bin:/usr/bin:/bin", HOME: tmp, TMPDIR: tmp, ...env };
      const res = await this.supervise(argv, childEnv, tmp, timeoutMs, signal);

      // 8. map
      const durationMs = Date.now() - started;
      const body = this.map(runId, durationMs, flow.steps, res, secretValues, timeoutMs);
      return { status: 200, body, audit: { after: { runId, status: (body as { status: string }).status } } };
    } finally {
      release();
      if (tmp) await rm(tmp, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  async scanHostKey(workspaceId: string, body: { host?: unknown; port?: unknown }, signal?: AbortSignal) {
    if (!(await this.available())) throw new ApiError(503, "test_run_unavailable", "host key scans are not configured on this server");
    const host = body.host;
    const port = body.port ?? 22;
    if (typeof host !== "string" || !/^[a-z0-9_:.%\[\]-]{1,253}$/i.test(host) || host.startsWith("-") ||
      typeof port !== "number" || !Number.isInteger(port) || port < 1 || port > 65535) {
      throw new ApiError(400, "bad_request", "enter an SSH host and a port between 1 and 65535");
    }
    const d = this.d;
    const release = d.limiter.tryAcquire(workspaceId);
    if (!release) throw new ApiError(429, "too_many_runs", "another run is in progress, retry shortly");
    let tmp: string | undefined;
    try {
      await checkEgress(host, { allowPrivate: d.allowPrivate ?? false, lookup: d.lookup });
      tmp = await mkdtemp(join(d.tmpRoot ?? tmpdir(), "nurtuo-hostkey-"));
      const timeoutMs = 10_000;
      const result = await this.supervise(["flow", "host-key", host, "--port", String(port), "--timeout", "10s", "--json"],
        { PATH: process.env.PATH ?? "/usr/local/bin:/usr/bin:/bin", HOME: tmp, TMPDIR: tmp }, tmp, timeoutMs, signal);
      if (result.code === 0 && !result.reason) {
        try {
          const parsed = JSON.parse(result.stdout);
          if (Array.isArray(parsed.keys) && parsed.keys.length > 0 && parsed.keys.every((key: unknown) =>
            key && typeof key === "object" && "type" in key && typeof key.type === "string" &&
            "fingerprintSha256" in key && typeof key.fingerprintSha256 === "string" && /^SHA256:[A-Za-z0-9+/]{43}$/.test(key.fingerprintSha256))) {
            return { keys: parsed.keys.map(({ type, fingerprintSha256 }: { type: string; fingerprintSha256: string }) => ({ type, fingerprintSha256 })) };
          }
        } catch { /* An unreadable result is a failed scan. */ }
      }
      throw new ApiError(400, "host_key_scan_failed", "could not fetch the SSH host key");
    } finally {
      release();
      if (tmp) await rm(tmp, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  private inputProperties(input: TestRunRequest["input"]): Record<string, string> {
    if (input === undefined) return {};
    const p = input && typeof input === "object" && !Array.isArray(input) ? input.properties : undefined;
    if (p === undefined) return {};
    if (!p || typeof p !== "object" || Array.isArray(p)) throw new ApiError(400, "bad_request", "input.properties must be an object of strings");
    const entries = Object.entries(p as Record<string, unknown>);
    if (entries.length > 64) throw new ApiError(400, "bad_request", "at most 64 input properties");
    const out: Record<string, string> = {};
    for (const [k, v] of entries) {
      if (!PROP_KEY.test(k)) throw new ApiError(400, "bad_request", `input property name "${k}" is not valid`);
      if (typeof v !== "string" || Buffer.byteLength(v, "utf8") > 4096) throw new ApiError(400, "bad_request", `input property "${k}" must be a string of at most 4 KiB`);
      out[k] = v;
    }
    return out;
  }

  private supervise(argv: string[], env: Record<string, string>, tmp: string, timeoutMs: number, signal?: AbortSignal) {
    const d = this.d;
    const stdoutLimit = d.stdoutLimit ?? 1.5 * 1024 * 1024, stderrLimit = d.stderrLimit ?? 64 * 1024;
    return new Promise<{ code: number | null; stdout: string; stderr: string; reason?: "deadline" | "output_too_large" | "aborted" }>((resolve) => {
      let child: ChildProcess;
      try { child = spawn(d.bin!, argv, { env, cwd: tmp, stdio: ["ignore", "pipe", "pipe"], shell: false }); }
      catch { resolve({ code: null, stdout: "", stderr: "failed to start the nurtuo binary" }); return; }
      let reason: "deadline" | "output_too_large" | "aborted" | undefined;
      const out: Buffer[] = [], err: Buffer[] = [];
      let outN = 0, errN = 0, killTimer: NodeJS.Timeout | undefined, exited = false;
      const kill = (why: typeof reason) => {
        if (exited || reason) return;
        reason = why;
        child.kill("SIGTERM");
        killTimer = setTimeout(() => { if (!exited) child.kill("SIGKILL"); }, d.killGraceMs ?? 5000);
      };
      child.stdout!.on("data", (b: Buffer) => { outN += b.length; if (outN > stdoutLimit) kill("output_too_large"); else out.push(b); });
      child.stderr!.on("data", (b: Buffer) => { if (errN < stderrLimit) { err.push(b.subarray(0, stderrLimit - errN)); errN += b.length; } });
      const deadline = setTimeout(() => kill("deadline"), timeoutMs + (d.deadlineSlackMs ?? 5000));
      let polling = false;
      const poll = setInterval(() => {
        if (polling) return;
        polling = true;
        dirSize(tmp).then((n) => { if (n > (d.spoolLimitBytes ?? 64 * 1024 * 1024)) kill("output_too_large"); }).finally(() => { polling = false; });
      }, d.spoolPollMs ?? 500);
      const onAbort = () => kill("aborted");
      if (signal?.aborted) onAbort(); else signal?.addEventListener("abort", onAbort, { once: true });
      const done = (code: number | null) => {
        if (exited) return;
        exited = true;
        clearTimeout(deadline); clearInterval(poll); if (killTimer) clearTimeout(killTimer);
        signal?.removeEventListener("abort", onAbort);
        resolve({ code, stdout: Buffer.concat(out).toString("utf8"), stderr: Buffer.concat(err).toString("utf8"), reason });
      };
      child.on("error", () => done(null));
      child.on("close", (code) => done(code));
    });
  }

  private map(runId: string, durationMs: number, flowSteps: readonly FlowStep[], res: { code: number | null; stdout: string; stderr: string; reason?: string }, secretValues: string[], timeoutMs: number) {
    const d = this.d;
    const clean = (s: string) => redact(s, secretValues);
    let cliSteps: CliStep[] | undefined;
    let error: CliError | undefined;
    let documents: Array<{ id: string; properties: Record<string, string>; payload: string }> | undefined;

    const lastJson = (s: string): any => {
      for (const line of s.split("\n").reverse()) { const t = line.trim(); if (t.startsWith("{")) { try { return JSON.parse(t); } catch { /* keep looking */ } } }
      return undefined;
    };
    if (res.code === 0) {
      const env = lastJson(res.stdout);
      if (env && Array.isArray(env.documents)) { documents = env.documents; cliSteps = Array.isArray(env.steps) ? env.steps : undefined; }
      else error = { code: "execution", message: "the nurtuo binary returned an unreadable result" };
    } else if (res.reason !== "deadline" && res.reason !== "output_too_large") {
      const e = lastJson(res.stderr);
      if (e?.error && typeof e.error.message === "string") { error = { code: String(e.error.code ?? "execution"), message: e.error.message }; cliSteps = Array.isArray(e.steps) ? e.steps : undefined; }
      else error = { code: "execution", message: res.stderr.trim().split("\n").pop() || `the nurtuo binary exited with code ${res.code}` };
    }

    let status: "succeeded" | "failed" | "stopped" = "succeeded";
    let stop: { status?: number; message?: string } | undefined;
    if (res.reason === "output_too_large") { status = "failed"; error = { code: "output_too_large", message: "output larger than 1 MiB; use `nurtuo flow run`" }; documents = undefined; }
    else if (res.reason === "deadline" || (error && error.code === "execution" && error.message.startsWith("timed out after"))) { status = "failed"; error = { code: "timeout", message: `the run timed out after ${timeoutMs / 1000} s` }; }
    else if (error?.code === "stopped") {
      status = "stopped";
      const m = /^flow stopped with status (\d+)(?:: (.*))?$/s.exec(error.message);
      stop = m ? { status: Number(m[1]), ...(m[2] ? { message: m[2] } : {}) } : {};
      error = undefined;
    } else if (error) status = "failed";

    if (status === "succeeded" && documents) {
      const total = documents.reduce((n, x) => n + Buffer.byteLength(x.payload ?? "", "base64"), 0);
      if (documents.length > (d.maxDocuments ?? 20) || total > (d.maxPayloadBytes ?? 1024 * 1024)) { status = "failed"; error = { code: "output_too_large", message: "output larger than 1 MiB or 20 documents; use `nurtuo flow run`" }; documents = undefined; }
    }

    const byId = new Map((cliSteps ?? []).map((s) => [s.id, s]));
    const stopStep = status === "stopped" ? [...(cliSteps ?? [])].reverse().find((s) => s.kind === "stop")?.id : undefined;
    // A failure raised before the engine ran (host key, password) names the connection: its connector steps are the failed ones.
    const failedConn = !cliSteps && error ? /connection "([^"]+)"/.exec(error.message)?.[1] : undefined;
    const steps = flowSteps.map((s) => {
      const t = byId.get(s.id);
      let st: StepStatus = "not_run";
      if (t) st = s.id === stopStep ? "stopped" : t.failed > 0 ? "error" : "ok";
      else if (failedConn && s.kind === "connector" && s.config?.connection === failedConn) st = "error";
      return { id: s.id, kind: s.kind, status: st, in: t?.in ?? 0, out: t?.out ?? 0, failed: t?.failed ?? 0, dropped: t?.dropped ?? 0 };
    });

    return {
      runId, status, durationMs, steps,
      ...(status === "succeeded" && documents ? { documents: documents.map((x) => ({ id: x.id, properties: x.properties ?? {}, size: Buffer.byteLength(x.payload ?? "", "base64"), payload: x.payload ?? "" })) } : {}),
      ...(error ? { error: { code: error.code, message: clean(error.message) } } : {}),
      ...(stop ? { stop: { ...stop, ...(stop.message ? { message: clean(stop.message) } : {}) } } : {}),
    };
  }
}
