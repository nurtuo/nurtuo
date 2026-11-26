// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { ConnectionCoordinator } from "./coordinator.js";

const gate = () => { let open!: () => void; const p = new Promise<void>((r) => { open = r; }); return { p, open }; };

test("aborted waiters leave the queue while the lock is held and a later waiter proceeds", async () => {
  const c = new ConnectionCoordinator();
  const held = gate();
  const holder = c.withWorkspace("w", () => held.p);
  const cancelled: Promise<unknown>[] = [];
  for (let i = 0; i < 50; i++) {
    const ac = new AbortController();
    const p = c.withWorkspace("w", async () => assert.fail("cancelled waiter must not run"), ac.signal);
    cancelled.push(assert.rejects(p, { name: "AbortError" }));
    ac.abort();
  }
  await Promise.all(cancelled);
  assert.equal(c.waiting("w"), 0);
  let ran = false;
  const live = c.withWorkspace("w", async () => { ran = true; });
  assert.equal(c.waiting("w"), 1);
  assert.equal(ran, false);
  held.open();
  await holder;
  await live;
  assert.equal(ran, true);
  assert.equal(c.waiting("w"), 0);
});

test("an already aborted signal never queues", async () => {
  const c = new ConnectionCoordinator();
  const held = gate();
  const holder = c.withWorkspace("w", () => held.p);
  await assert.rejects(c.withWorkspace("w", async () => {}, AbortSignal.abort()), { name: "AbortError" });
  assert.equal(c.waiting("w"), 0);
  held.open();
  await holder;
});

test("an aborted holder outside a critical section gives up the lock", async () => {
  const c = new ConnectionCoordinator();
  const ac = new AbortController();
  const stall = gate();
  const holder = c.withWorkspace("w", () => stall.p, ac.signal);
  let ran = false;
  const next = c.withWorkspace("w", async () => { ran = true; });
  ac.abort();
  await next;
  assert.equal(ran, true);
  stall.open();
  await holder;
});

test("critical sections keep the lock after abort until they finish", async () => {
  const c = new ConnectionCoordinator();
  const ac = new AbortController();
  const write = gate();
  const inside = gate();
  const holder = c.withWorkspace("w", (lease) => lease.critical(async () => { inside.open(); await write.p; }), ac.signal);
  await inside.p;
  const order: string[] = [];
  const next = c.withWorkspace("w", async () => { order.push("next"); });
  ac.abort();
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(order.length, 0);
  order.push("write done");
  write.open();
  await holder;
  await next;
  assert.deepEqual(order, ["write done", "next"]);
});

test("serialization is FIFO and the lock is released on error", async () => {
  const c = new ConnectionCoordinator();
  const order: number[] = [];
  const a = c.withWorkspace("w", async () => { await new Promise((r) => setTimeout(r, 10)); order.push(1); throw new Error("x"); });
  const b = c.withWorkspace("w", async () => { order.push(2); });
  await assert.rejects(a);
  await b;
  assert.deepEqual(order, [1, 2]);
});
