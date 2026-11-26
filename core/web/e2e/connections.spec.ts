// SPDX-License-Identifier: AGPL-3.0-only
import { expect, test } from "@playwright/test";

test("clearing the SFTP root sends an explicit edit and preserves the password", async ({ page }) => {
  const connection = {
    id: "sftp",
    connector: "sftp",
    settings: { host: "example.test", port: 22, user: "editor", rootPath: "/old", hostKeyFingerprint: "SHA256:test" },
    secrets: { password: "set" },
  };
  await page.addInitScript(() => localStorage.setItem("nurtuo.cp.session", "1"));
  await page.route("**/v1/auth/me", (route) => route.fulfill({ json: {
    user: { email: "editor@example.test" },
    workspaces: [{ id: "one", name: "One", role: "owner" }],
    csrfToken: "test-csrf",
  } }));
  await page.route("**/v1/workspaces/one/connections", (route) => route.fulfill({ json: { items: [connection] } }));
  await page.route("**/v1/workspaces/one/connections/sftp", (route) => route.fulfill({ json: connection }));
  await page.goto("/");
  await page.getByRole("button", { name: "Connections", exact: true }).click();
  await page.getByText("sftp", { exact: true }).click();
  const root = page.getByLabel("rootPath", { exact: true });
  await expect(root).toHaveValue("/old");
  await root.fill("");
  const save = page.waitForRequest((request) => request.method() === "PUT" && request.url().endsWith("/connections/sftp"));
  await page.getByRole("button", { name: "Save connection" }).click();
  const body = (await save).postDataJSON();
  expect(body.settings.rootPath).toBe("");
  expect(body).not.toHaveProperty("credentials");
});

test("Fetch leaves the saved key unchanged and Trust applies the key for the scanned host and port", async ({ page }) => {
  const oldKey = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
  const newKey = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB";
  const connection = {
    id: "sftp", connector: "sftp",
    settings: { host: "example.test", port: 2222, user: "editor", hostKeyFingerprint: oldKey },
    secrets: { password: "set" },
  };
  const writes: unknown[] = [];
  await page.addInitScript(() => localStorage.setItem("nurtuo.cp.session", "1"));
  await page.route("**/v1/auth/me", (route) => route.fulfill({ json: {
    user: { email: "editor@example.test" },
    workspaces: [{ id: "one", name: "One", role: "owner" }],
    csrfToken: "test-csrf",
  } }));
  await page.route("**/v1/workspaces/one/connections", (route) => route.fulfill({ json: { items: [connection] } }));
  await page.route("**/v1/workspaces/one/connections/sftp", (route) => {
    if (route.request().method() === "PUT") writes.push(route.request().postDataJSON());
    return route.fulfill({ json: connection });
  });
  await page.route("**/v1/workspaces/one/connections/host-key-scan", (route) => {
    expect(route.request().postDataJSON()).toEqual({ host: "example.test", port: 2222 });
    return route.fulfill({ json: { keys: [{ type: "ssh-ed25519", fingerprintSha256: newKey }] } });
  });
  await page.goto("/");
  await page.getByRole("button", { name: "Connections", exact: true }).click();
  await page.getByText("sftp", { exact: true }).click();
  const fingerprint = page.getByLabel("Host key", { exact: true });
  const trust = page.getByRole("button", { name: "Trust this key" });
  await page.getByRole("button", { name: "Fetch host key" }).click();
  await expect(trust).toBeVisible();
  await expect(fingerprint).toHaveValue(oldKey);
  expect(writes).toEqual([]);

  const host = page.getByLabel("host", { exact: true });
  await host.fill("other.test");
  await expect(trust).toBeHidden();
  await host.fill("example.test");
  const port = page.getByLabel("port", { exact: true });
  await port.fill("22");
  await expect(trust).toBeHidden();
  await port.fill("2222");
  await trust.click();
  await expect(fingerprint).toHaveValue(newKey);
  expect(writes).toEqual([]);
});
