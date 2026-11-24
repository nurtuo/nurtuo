// SPDX-License-Identifier: AGPL-3.0-only
import type { Flow } from "@nurtuo/flow-schema";
import { describe, expect, it } from "vitest";
import type { TestRunResponse } from "../api/controlPlane";
import { IDLE, failureHint, previewOf, reconcile, stepStatuses, hashFlow, type RunState } from "./runState";

const flow = (): Flow => ({
  schema: "nurtuo.flow/v1", id: "f", name: "f", revision: 1,
  steps: [
    { id: "start-1", kind: "start", config: { type: "manual" } },
    { id: "connector-1", kind: "connector", config: { connector: "sftp", operation: "GET", connection: "c" } },
    { id: "stop-1", kind: "stop", config: { status: 404 } },
    { id: "after", kind: "returnDocuments", config: {} },
  ],
  edges: { "start-1:default": "connector-1", "connector-1:default": "stop-1" },
});
const step = (id: string, status: "ok" | "error" | "stopped" | "not_run") => ({ id, kind: "x", status, in: 1, out: 1, failed: 0, dropped: 0 });
const done = (f: Flow, result: Partial<TestRunResponse>): RunState => ({
  phase: "done", flowHash: hashFlow(f),
  result: { runId: "r", status: "succeeded", durationMs: 1, steps: [], documents: [], ...result },
});

describe("runState", () => {
  it("maps step statuses; steps absent from the result are not_run; Stop is stopped", () => {
    const f = flow();
    const m = stepStatuses(done(f, { status: "stopped", steps: [step("start-1", "ok"), step("connector-1", "ok"), step("stop-1", "stopped")] }), f);
    expect([...m]).toEqual([["start-1", "ok"], ["connector-1", "ok"], ["stop-1", "stopped"], ["after", "not_run"]]);
  });

  it("keeps an error status (failed run)", () => {
    const f = flow();
    expect(stepStatuses(done(f, { status: "failed", steps: [step("connector-1", "error")] }), f).get("connector-1")).toBe("error");
  });

  it("is cleared when the flow changes, kept when it does not", () => {
    const f = flow();
    const s = done(f, {});
    expect(reconcile(s, f)).toBe(s);
    const edited = { ...f, name: "other" };
    expect(reconcile(s, edited)).toEqual(IDLE);
    expect(stepStatuses(s, edited).size).toBe(0);
    expect(reconcile({ phase: "running", flowHash: hashFlow(f) }, edited)).toEqual(IDLE);
  });

  it("previews UTF-8 text, reports binary, truncates large text", () => {
    const b64 = (b: Uint8Array) => Buffer.from(b).toString("base64");
    const doc = (payload: string) => ({ id: "d", properties: {}, payload });
    expect(previewOf(doc(Buffer.from("héllo").toString("base64")))).toEqual({ kind: "text", text: "héllo", truncated: false, size: 6 });
    expect(previewOf(doc(b64(new Uint8Array([0xff, 0xfe, 0x00]))))).toEqual({ kind: "binary", size: 3 });
    const big = previewOf(doc(Buffer.from("a".repeat(70 * 1024)).toString("base64")));
    expect(big.kind === "text" && big.truncated && big.text.length === 64 * 1024).toBe(true);
  });

  it("hints for host key mismatch and oversized output", () => {
    const f = flow();
    const r = (code: string, message: string) => (done(f, { status: "failed", error: { code, message } }) as { result: TestRunResponse }).result;
    expect(failureHint(r("execution", "ssh connect: host key mismatch: got A, want B"))).toMatch(/key changed/);
    expect(failureHint(r("output_too_large", "x"))).toMatch(/1 MiB/);
    expect(failureHint(r("execution", "boom"))).toBeNull();
  });
});
