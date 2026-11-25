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

it("signs in, loads available workspaces and restores CSRF in a fresh client", async () => {
  const workspaces = [
    { id: "one", slug: "one", name: "One", role: "owner", permissions: [] },
    { id: "two", slug: "two", name: "Two", role: "viewer", permissions: [] },
  ];
  const me = { user: { email: "editor@example.test" }, workspaces, csrfToken: "session-csrf" };
  const first = fake((url) => json(url.endsWith("/login") ? { csrfToken: "session-csrf" } : me));
  expect(await first.cp.login(me.user.email, "test-password")).toEqual(me);
  expect(first.calls.map(({ url }) => url)).toEqual(["/v1/auth/login", "/v1/auth/me"]);
  expect(first.calls.every(({ init }) => init.credentials === "same-origin")).toBe(true);

  const reload = fake(() => json(me));
  expect(reload.cp.csrfToken).toBeNull();
  expect(await reload.cp.me()).toEqual(me);
  expect(reload.cp.csrfToken).toBe("session-csrf");
});

it("sends CSRF on logout and clears it only after revocation is confirmed", async () => {
  let status = 500;
  const { cp, calls } = fake((url) => url.endsWith("/me")
    ? json({ user: {}, workspaces: [], csrfToken: "restored-csrf" })
    : new Response(null, { status }));
  await cp.me();
  await expect(cp.logout()).rejects.toMatchObject({ status: 500 });
  expect(cp.csrfToken).toBe("restored-csrf");
  status = 204;
  await cp.logout();
  expect(cp.csrfToken).toBeNull();
  expect(calls.slice(1).map(({ init }) => [init.method, new Headers(init.headers).get("x-nurtuo-csrf")]))
    .toEqual([["POST", "restored-csrf"], ["POST", "restored-csrf"]]);
});
