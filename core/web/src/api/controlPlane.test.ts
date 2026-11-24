// SPDX-License-Identifier: AGPL-3.0-only
import { describe, expect, it } from "vitest";
import { ControlPlane, OfflineError } from "./controlPlane";

interface Call { url: string; init: RequestInit }
function fake(handler: (url: string, init: RequestInit) => Response | Promise<Response>) {
  const calls: Call[] = [];
  const fetchFn = async (url: string, init?: RequestInit) => { calls.push({ url, init: init ?? {} }); return handler(url, init ?? {}); };
  return { calls, cp: new ControlPlane(fetchFn) };
}
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

describe("ControlPlane client", () => {
  it("me() is null on 401", async () => {
    const { cp } = fake(() => json({ error: { code: "unauthorized", message: "no" } }, 401));
    expect(await cp.me()).toBeNull();
  });

  it("network failures and proxy errors are offline", async () => {
    const down = fake(() => { throw new TypeError("fetch failed"); });
    await expect(down.cp.me()).rejects.toBeInstanceOf(OfflineError);
    const proxy = fake(() => new Response("Bad gateway", { status: 502 }));
    await expect(proxy.cp.me()).rejects.toBeInstanceOf(OfflineError);
  });
});
