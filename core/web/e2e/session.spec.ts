// SPDX-License-Identifier: AGPL-3.0-only
import { expect, test } from "@playwright/test";

const me = {
  user: { email: "editor@example.test" },
  workspaces: [
    { id: "one", name: "One", role: "owner" },
    { id: "two", name: "Two", role: "viewer" },
  ],
  csrfToken: "csrf-login",
};

test("restoration and sign-in cannot overlap, and sign-in can retry", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("nurtuo.cp.session", "1"));
  const restoreStarted = Promise.withResolvers<void>();
  const releaseRestore = Promise.withResolvers<void>();
  const loginStarted = Promise.withResolvers<void>();
  const releaseLogin = Promise.withResolvers<void>();
  let loginCount = 0;
  await page.route("**/v1/auth/me", async (route) => {
    if (loginCount === 0) {
      restoreStarted.resolve();
      await releaseRestore.promise;
      await route.abort();
    } else {
      await route.fulfill({ json: me });
    }
  });
  await page.route("**/v1/auth/login", async (route) => {
    loginCount++;
    if (loginCount === 1) {
      await route.abort();
    } else {
      loginStarted.resolve();
      await releaseLogin.promise;
      await route.fulfill({ json: { csrfToken: me.csrfToken } });
    }
  });
  await page.goto("/");
  await restoreStarted.promise;
  const button = page.getByRole("button", { name: "Sign in" });
  await expect(button).toBeDisabled();
  releaseRestore.resolve();
  await expect(button).toBeEnabled();
  await expect(page.getByRole("alert")).toContainText("Could not restore your session");
  await page.getByLabel("Email").fill(me.user.email);
  await page.getByLabel("Password").fill("test-password");
  await button.click();
  await expect(page.getByRole("alert")).toContainText("Control plane unreachable");
  await expect(button).toBeEnabled();
  await page.getByLabel("Password").fill("test-password");
  await button.click();
  await loginStarted.promise;
  await expect(button).toBeDisabled();
  releaseLogin.resolve();
  const workspace = page.getByLabel("Workspace");
  await expect(workspace).toHaveValue("one");
  await workspace.selectOption("two");
  await expect(workspace).toHaveValue("two");
  expect(loginCount).toBe(2);
});

test("a session without workspaces can still log out", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("nurtuo.cp.session", "1"));
  await page.route("**/v1/auth/me", (route) => route.fulfill({ json: { ...me, workspaces: [] } }));
  await page.route("**/v1/auth/logout", (route) => route.fulfill({ status: 204 }));
  await page.goto("/");
  await expect(page.getByText("No workspaces available")).toBeVisible();
  await expect(page.getByLabel("Workspace")).toHaveCount(0);
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page.getByRole("button", { name: "Sign in" })).toBeEnabled();
});

test("reload restores CSRF and logout retains the session until revocation succeeds", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("nurtuo.cp.session", "1"));
  let restores = 0;
  const headers: (string | undefined)[] = [];
  await page.route("**/v1/auth/me", (route) => route.fulfill({
    json: { ...me, csrfToken: `csrf-restore-${++restores}` },
  }));
  await page.route("**/v1/auth/logout", (route) => {
    headers.push(route.request().headers()["x-nurtuo-csrf"]);
    return route.fulfill({ status: headers.length === 1 ? 500 : 204 });
  });
  await page.goto("/");
  await expect(page.getByLabel("Workspace")).toBeVisible();
  await page.reload();
  const logout = page.getByRole("button", { name: "Log out" });
  await logout.click();
  await expect(page.getByRole("alert")).toContainText("Could not revoke");
  await expect(page.getByLabel("Workspace")).toBeVisible();
  await logout.click();
  await expect(page.getByRole("button", { name: "Sign in" })).toBeEnabled();
  expect(headers).toEqual(["csrf-restore-2", "csrf-restore-2"]);
});
