// SPDX-License-Identifier: AGPL-3.0-only
import { expect, test } from "@playwright/test";
import { fileURLToPath } from "node:url";

test.skip(process.env.NURTUO_E2E_REBEX !== "1", "manual Rebex smoke");
test.use({ screenshot: "off", trace: "off", video: "off" });

test("Run the sample against the public Rebex demo", async ({ page }) => {
  const password = process.env.NURTUO_E2E_REBEX_PASSWORD;
  const fingerprint = process.env.NURTUO_E2E_REBEX_FINGERPRINT;
  if (!password || !fingerprint) throw new Error("Set NURTUO_E2E_REBEX_PASSWORD and NURTUO_E2E_REBEX_FINGERPRINT from the demo page.");

  await page.goto("/");
  await page.getByLabel("Email").fill(process.env.NURTUO_E2E_CP_EMAIL!);
  await page.getByLabel("Password").fill(process.env.NURTUO_E2E_CP_PASSWORD!);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.getByRole("button", { name: "Connections" }).click();
  const connection = page.getByRole("dialog", { name: "Connections" });
  await connection.getByLabel("Connection id").fill("rebex-demo");
  await connection.getByLabel("host", { exact: true }).fill("test.rebex.net");
  await connection.getByLabel("user", { exact: true }).fill("demo");
  await connection.getByLabel("password").fill(password);
  await connection.getByRole("button", { name: "Fetch host key" }).click();
  await expect(connection.getByText(fingerprint, { exact: true })).toBeVisible();
  await connection.getByRole("button", { name: "Trust this key" }).click();
  await connection.getByRole("button", { name: "Save connection" }).click();
  await expect(connection.locator('[data-connection="rebex-demo"]')).toBeVisible();
  await connection.getByRole("button", { name: "Close" }).click();

  await page.locator('input[type="file"]').setInputFiles(fileURLToPath(new URL("../../../examples/flows/sftp-rebex-demo.flow.json", import.meta.url)));
  await page.getByRole("button", { name: "Run", exact: true }).click();
  const run = page.getByRole("dialog", { name: "Run flow" });
  await run.getByLabel("property value 0").fill("/readme.txt");
  await run.getByRole("button", { name: "Run", exact: true }).click();
  await expect(page.getByTestId("run-status")).toHaveText("succeeded", { timeout: 45_000 });
  await expect(page.getByTestId("doc-preview")).toBeVisible();
  await expect(page.locator('[data-run-status="ok"]')).toHaveCount(3);
});
