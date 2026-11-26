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
const hdr = (c: Call, k: string) => (c.init.headers as Record<string, string>)[k];

describe("ControlPlane client", () => {
  it("sends the CSRF header on mutations only, with same-origin credentials", async () => {
    const { cp, calls } = fake((url) => (url.endsWith("/login") ? json({ csrfToken: "tok" }) : url.endsWith("/me") ? json({ user: null, csrfToken: "tok", workspaces: [] }) : json({ items: [] })));
    await cp.login("a@b.c", "pw");
    await cp.listConnections("ws1");
    const login = calls[0]!;
    expect(hdr(login, "x-nurtuo-csrf")).toBeUndefined(); // no token yet
    const get = calls.find((c) => c.url.endsWith("/connections"))!;
    expect(hdr(get, "x-nurtuo-csrf")).toBeUndefined();
    expect(get.init.credentials).toBe("same-origin");
    await cp.putConnection("ws1", "c1", { connector: "sftp", settings: {} });
    expect(hdr(calls.at(-1)!, "x-nurtuo-csrf")).toBe("tok");
  });

  it("restores the CSRF token from /v1/auth/me after a reload", async () => {
    const { cp, calls } = fake((url) => (url === "/v1/auth/me" ? json({ user: { email: "e" }, csrfToken: "restored", workspaces: [{ id: "w" }] }) : new Response(null, { status: 204 })));
    expect(cp.csrfToken).toBeNull();
    const me = await cp.me();
    expect(me?.workspaces).toHaveLength(1);
    await cp.putConnection("w", "c1", { connector: "sftp", settings: {}, credentials: { password: "v" } });
    expect(hdr(calls.at(-1)!, "x-nurtuo-csrf")).toBe("restored");
  });

  it("me() is null on 401", async () => {
    const { cp } = fake(() => json({ error: { code: "unauthorized", message: "no" } }, 401));
    expect(await cp.me()).toBeNull();
  });

  it("sends the password in the write-only credentials member of a single PUT", async () => {
    const { cp, calls } = fake(() => new Response(null, { status: 204 }));
    await cp.putConnection("w", "c1", { connector: "sftp", settings: {}, credentials: { password: "test-value" } });
    expect(calls).toHaveLength(1);
    expect(calls[0]!.url).toBe("/v1/workspaces/w/connections/c1");
    expect(JSON.parse(String(calls[0]!.init.body))).toEqual({ connector: "sftp", settings: {}, credentials: { password: "test-value" } });
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
