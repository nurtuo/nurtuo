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
