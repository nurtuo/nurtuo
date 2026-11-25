// SPDX-License-Identifier: AGPL-3.0-only
// TestRunner: runs an inline flow through `nurtuo flow run --json` as a short-lived child process
// (canvas-run spec D7). One process per run, a per-run temp dir, a from-scratch child environment.
import { spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import { access, constants, lstat, mkdtemp, readdir, rm, writeFile } from "node:fs/promises";
import { isIP } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { hasErrors, validateFlow } from "@nurtuo/flow-validate";
import { ApiError } from "../domain.js";
import { binding, checkSecretPlacement } from "../connections/binding.js";
import { ConnectionCoordinator } from "../connections/coordinator.js";
import type { ConnectionRepository } from "../connections/repository.js";
import { envVar } from "../connections/env-var.js";
import type { SecretStore } from "../connections/secrets.js";
import { credentialSlots, secretRefs, validateConnection, validateConnectionId } from "../connections/validate.js";
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
  coordinator?: ConnectionCoordinator;
  limiter: RunLimiter;
  /** Absolute path of the nurtuo binary (NURTUO_FLOW_RUN_BIN). */
  bin?: string;
  allowPrivate?: boolean;
  lookup?: Lookup;
  platform?: NodeJS.Platform;
  tmpRoot?: string;
  /** Test knobs; defaults are the spec's limits. */
  killGraceMs?: number;
  scanTimeoutMs?: number;
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
  for (const v of values) if (v) out = out.split(v).join("[redacted]");
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

async function untilAbort<T>(signal: AbortSignal, work: () => Promise<T>): Promise<T> {
  signal.throwIfAborted();
  let onAbort!: () => void;
  const aborted = new Promise<never>((_, reject) => {
    onAbort = () => reject(signal.reason);
    signal.addEventListener("abort", onAbort, { once: true });
  });
  try {
    return await Promise.race([aborted, work()]);
  } finally {
    signal.removeEventListener("abort", onAbort);
  }
}

export class TestRunner {
  private coordinator: ConnectionCoordinator;

  constructor(private d: TestRunDeps) { this.coordinator = d.coordinator ?? new ConnectionCoordinator(); }

  async available(): Promise<boolean> {
    if ((this.d.platform ?? process.platform) === "win32" || !this.d.bin) return false;
    try { await access(this.d.bin, constants.X_OK); return true; } catch { return false; }
  }

  async run(workspaceId: string, req: TestRunRequest, signal?: AbortSignal, authorize?: () => Promise<void>): Promise<RunOutcome> {
    const d = this.d;
    if (!(await this.available())) throw new ApiError(503, "test_run_unavailable", "test runs are not configured on this server");

    // 1. limits
    const timeoutMs = req.timeoutMs === undefined ? DEFAULT_TIMEOUT_MS : req.timeoutMs;
    if (typeof timeoutMs !== "number" || !Number.isInteger(timeoutMs) || timeoutMs < MIN_TIMEOUT_MS || timeoutMs > MAX_TIMEOUT_MS) throw new ApiError(400, "bad_request", `timeoutMs must be an integer between ${MIN_TIMEOUT_MS} and ${MAX_TIMEOUT_MS}`);
    const props = this.inputProperties(req.input);

    const release = d.limiter.tryAcquire(workspaceId);
    if (!release) throw new ApiError(429, "too_many_runs", "another run is in progress, retry shortly");
    const started = Date.now();
    const runId = randomUUID();
    const deadlineController = new AbortController();
    const deadline = deadlineController.signal;
    const timer = setTimeout(() => deadlineController.abort(), timeoutMs);
    const operation = signal ? AbortSignal.any([signal, deadline]) : deadline;
    let tmp: string | undefined;
    let steps: FlowStep[] = [];
    try {
      operation.throwIfAborted();
      const diags = validateFlow(req.flow);
      if (hasErrors(diags)) return { status: 400, body: { error: { code: "invalid_flow", message: "the flow is not valid", diagnostics: diags } } };
      steps = (req.flow as { steps: FlowStep[] }).steps;
      const uses = checkFlowPolicy(steps);

      // 3. connections and secrets of this workspace only
      const ids = [...new Set(uses.map((u) => u.connection))];
      const { conns, env, secretValues } = await untilAbort(operation, () => this.coordinator.withWorkspace(workspaceId, async () => {
        operation.throwIfAborted();
        await authorize?.();
        operation.throwIfAborted();
        const conns = [];
        for (const id of ids) {
          operation.throwIfAborted();
          const reconciled = await this.coordinator.reconcile(d.connections, d.secrets, workspaceId, id);
          operation.throwIfAborted();
          const c = reconciled ?? await d.connections.get(workspaceId, id);
          operation.throwIfAborted();
          if (!c) throw new ApiError(404, "connection_not_found", `connection "${id}" not found`);
          if (uses.some((use) => use.connection === id && use.connector !== c.connector)) {
            throw new ApiError(400, "connection_type_mismatch", `connection "${id}" does not match the connector step`);
          }
          if (c.connector === "sftp" && c.settings.hostKeyPolicy !== "fingerprint") throw new ApiError(409, "dial_enforcement_unavailable", "test runs require a pinned SSH host key fingerprint");
          try {
            validateConnectionId(c.id);
            if (c.id !== id) throw new Error("connection id mismatch");
            checkSecretPlacement(c.connector, c.settings, true);
            validateConnection(c.connector, c.settings);
            if (c.settings.passphrase && !c.settings.privateKey) throw new Error("invalid credential set");
          } catch {
            throw new ApiError(409, "credential_unbound", "stored connection has invalid settings");
          }
          conns.push(c);
        }
        operation.throwIfAborted();
        const env: Record<string, string> = {};
        const secretValues: string[] = [];
        for (const c of conns) {
          if (!c.incarnation) throw new ApiError(409, "credential_unbound", "re-enter the connection credentials");
          for (const { field, name } of secretRefs(c.connector, c.settings)) {
            const key = envVar(name);
            if (Object.hasOwn(env, key)) throw new ApiError(409, "credential_unbound", "credential reference is shared");
            const value = d.secrets.acquire(workspaceId, name, binding(c, field));
            if (value === undefined) throw new ApiError(409, "secret_missing", `credential "${field}" of connection "${c.id}" is not set; open Connections and enter it again`);
            env[key] = value;
            secretValues.push(value);
            if (c.connector === "http" && field === "password") {
              const auth = credentialSlots(c.connector, c.settings);
              secretValues.push(Buffer.from(`${String(auth.username)}:${value}`).toString("base64"));
            }
          }
        }
        return { conns, env, secretValues };
      }));
      // 4. egress
      const dialPlan = { http: new Map<string, string>(), postgres: new Map<string, string>() };
      const settingsForChild = [];
      for (const c of conns) {
        const url = c.connector === "http" ? new URL(String(c.settings.baseUrl)) : undefined;
        const hostname = url?.hostname ?? String(c.settings.host);
        const address = await checkEgress(hostname, {
          allowPrivate: d.allowPrivate ?? false, lookup: d.lookup, signal: operation,
        });
        if (c.connector === "http" || c.connector === "postgres") {
          const port = url ? url.port || (url.protocol === "https:" ? 443 : 80) : c.settings.port ?? 5432;
          const host = isIP(hostname) === 6 ? `[${hostname}]` : hostname;
          dialPlan[c.connector].set(`${host}:${port}`, address);
        }
        settingsForChild.push(url ? { ...c.settings, baseUrl: url.href }
          : c.connector === "sftp" ? { ...c.settings, host: address } : c.settings);
      }
      const pinned = dialPlan.http.size + dialPlan.postgres.size > 0;

      // 5. temp dir with the flow and non-secret connection files
      tmp = await mkdtemp(join(d.tmpRoot ?? tmpdir(), "nurtuo-testrun-"));
      const flowFile = join(tmp, "flow.json");
      await writeFile(flowFile, JSON.stringify(req.flow), { mode: 0o600 });
      const argv = ["flow", pinned ? "run-pinned" : "run", flowFile, "--json", "--max-output-bytes", String(d.maxPayloadBytes ?? 1024 * 1024), "--max-documents", String(d.maxDocuments ?? 20)];
      for (const [index, c] of conns.entries()) {
        const f = join(tmp, `${c.id}.connection.json`);
        await writeFile(f, JSON.stringify({ schema: "nurtuo.connection/v1", id: c.id, connector: c.connector, settings: settingsForChild[index]! }), { mode: 0o600 });
        argv.push("--connection", f);
      }
      for (const [k, v] of Object.entries(props)) argv.push("--set", `${k}=${v}`);

      // 6-7. spawn and supervise
      operation.throwIfAborted();
      argv.push("--timeout", `${Math.max(1, timeoutMs - (Date.now() - started))}ms`);
      const childEnv: Record<string, string> = { PATH: process.env.PATH ?? "/usr/local/bin:/usr/bin:/bin", HOME: tmp, TMPDIR: tmp, ...env };
      if (pinned) childEnv.NURTUO_TEST_RUN_DIAL_PLAN = JSON.stringify({
        http: Object.fromEntries(dialPlan.http), postgres: Object.fromEntries(dialPlan.postgres),
      });
      const res = await this.supervise(argv, childEnv, tmp, operation, deadline);

      // 8. map
      const durationMs = Date.now() - started;
      const body = this.map(runId, durationMs, steps, res, secretValues, timeoutMs);
      return { status: 200, body, audit: { after: { runId, status: (body as { status: string }).status } } };
    } catch (error) {
      if (!deadline.aborted || signal?.aborted) throw error;
      const body = this.map(runId, Date.now() - started, steps, { code: null, stdout: "", stderr: "", reason: "deadline" }, [], timeoutMs);
      return { status: 200, body, audit: { after: { runId, status: body.status } } };
    } finally {
      clearTimeout(timer);
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
    const started = Date.now();
    const timeoutMs = d.scanTimeoutMs ?? 10_000;
    const deadlineController = new AbortController();
    const deadline = deadlineController.signal;
    const timer = setTimeout(() => deadlineController.abort(), timeoutMs);
    const operation = signal ? AbortSignal.any([signal, deadline]) : deadline;
    try {
      const address = await checkEgress(host, { allowPrivate: d.allowPrivate ?? false, lookup: d.lookup, signal: operation });
      tmp = await mkdtemp(join(d.tmpRoot ?? tmpdir(), "nurtuo-hostkey-"));
      operation.throwIfAborted();
      const remaining = Math.max(1, timeoutMs - (Date.now() - started));
      const result = await this.supervise(["flow", "host-key", address, "--port", String(port), "--timeout", `${remaining}ms`, "--json"],
        { PATH: process.env.PATH ?? "/usr/local/bin:/usr/bin:/bin", HOME: tmp, TMPDIR: tmp }, tmp, operation, deadline);
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
    } catch (error) {
      if (deadline.aborted && !signal?.aborted) throw new ApiError(400, "host_key_scan_failed", "SSH host key scan timed out");
      throw error;
    } finally {
      clearTimeout(timer);
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

  private supervise(argv: string[], env: Record<string, string>, tmp: string, signal: AbortSignal, deadline: AbortSignal) {
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
      let polling = false;
      const poll = setInterval(() => {
        if (polling) return;
        polling = true;
        dirSize(tmp).then((n) => { if (n > (d.spoolLimitBytes ?? 64 * 1024 * 1024)) kill("output_too_large"); }).finally(() => { polling = false; });
      }, d.spoolPollMs ?? 500);
      const onAbort = () => kill(deadline.aborted ? "deadline" : "aborted");
      if (signal?.aborted) onAbort(); else signal?.addEventListener("abort", onAbort, { once: true });
      const done = (code: number | null) => {
        if (exited) return;
        exited = true;
        clearInterval(poll); if (killTimer) clearTimeout(killTimer);
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

    const stopStep = [...(cliSteps ?? [])].reverse().find((s) => s.kind === "stop")?.id;
    let status: "succeeded" | "failed" | "stopped" = "succeeded";
    let stop: { status?: number; message?: string } | undefined;
    if (res.reason === "output_too_large" || error?.code === "output_too_large") { status = "failed"; error = { code: "output_too_large", message: "output larger than 1 MiB; use `nurtuo flow run`" }; documents = undefined; }
    else if (res.reason === "deadline" || (error && error.code === "execution" && error.message.startsWith("timed out after"))) { status = "failed"; error = { code: "timeout", message: `the run timed out after ${timeoutMs / 1000} s` }; }
    else if (error?.code === "stopped") {
      status = "stopped";
      const m = /^flow stopped with status (\d+)(?:: (.*))?$/s.exec(error.message);
      stop = m ? { status: Number(m[1]), ...(m[2] ? { message: m[2] } : {}) } : {};
      error = undefined;
    } else if (error) status = "failed";
    else if (stopStep) { status = "stopped"; stop = {}; }

    if (status === "succeeded" && documents) {
      const total = documents.reduce((n, x) => n + Buffer.byteLength(x.payload ?? "", "base64"), 0);
      if (documents.length > (d.maxDocuments ?? 20) || total > (d.maxPayloadBytes ?? 1024 * 1024)) { status = "failed"; error = { code: "output_too_large", message: "output larger than 1 MiB or 20 documents; use `nurtuo flow run`" }; documents = undefined; }
    }

    if (status === "succeeded" && documents && secretValues.length > 0) {
      const needles = secretValues.map((value) => Buffer.from(value));
      const containsCredential = documents.some((doc) => {
        const payload = Buffer.from(doc.payload ?? "", "base64");
        const fields = [doc.id, ...Object.entries(doc.properties ?? {}).flat()];
        return needles.some((value) => payload.includes(value)) || fields.some((field) => clean(field) !== field);
      });
      if (containsCredential) {
        status = "failed";
        error = { code: "credential_in_result", message: "the returned documents contained a workspace credential" };
        documents = undefined;
      }
    }

    const byId = new Map((cliSteps ?? []).map((s) => [s.id, s]));
    // A failure raised before the engine ran (host key, password) names the connection: its connector steps are the failed ones.
    const failedConn = !cliSteps && error ? /connection "([^"]+)"/.exec(error.message)?.[1] : undefined;
    const steps = flowSteps.map((s) => {
      const t = byId.get(s.id);
      let st: StepStatus = "not_run";
      if (t) st = status === "stopped" && s.id === stopStep ? "stopped" : t.failed > 0 ? "error" : "ok";
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
