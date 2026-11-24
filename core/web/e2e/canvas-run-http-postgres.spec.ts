// SPDX-License-Identifier: AGPL-3.0-only
import { expect, test, type Page } from "@playwright/test";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";

const env = (key: string) => process.env[key] ?? "";
const fixture = new URL("../../../examples/flows/get-employee-test-run.flow.json", import.meta.url);
test.use({ trace: "off", screenshot: "off" });
test.skip(!env("NURTUO_E2E_POSTGRES_PORT"), "requires the local PostgreSQL employee fixture");

async function saveConnection(page: Page, id: string, connector: string, fields: Record<string, string>) {
  await page.getByRole("button", { name: "Connections" }).click();
  const dialog = page.getByRole("dialog", { name: "Connections" });
  await dialog.getByLabel("Connection id").fill(id);
  await dialog.getByLabel("Connector", { exact: true }).selectOption(connector);
  for (const [name, value] of Object.entries(fields)) {
    const control = dialog.getByLabel(name, { exact: true });
    if (name === "type" || name === "sslMode") await control.selectOption(value);
    else await control.fill(value);
  }
  await dialog.getByRole("button", { name: "Save connection" }).click();
  const row = dialog.locator(`[data-connection="${id}"]`);
  await expect(row).toBeVisible();
  await row.locator("code").click();
  await expect(dialog.getByLabel(connector === "http" ? "token" : "password", { exact: true })).toHaveValue("");
  await dialog.getByRole("button", { name: "Close" }).click();
}

async function runFlow(page: Page, result: string) {
  await page.getByRole("button", { name: "Run", exact: true }).click();
  await page.getByRole("dialog", { name: "Run flow" }).getByRole("button", { name: "Run", exact: true }).click();
  await expect(page.getByTestId("run-status")).toHaveText(result, { timeout: 40_000 });
}

test("saved HTTP and PostgreSQL operations run the employee flow and show failed authentication", async ({ page }) => {
  test.setTimeout(60_000);
  const token = randomBytes(24).toString("hex");
  let allow = true;
  let requests = 0;
  const server = createServer((req, res) => {
    if (!allow || req.method !== "GET" || req.headers.authorization !== `Bearer ${token}`) {
      res.writeHead(401).end();
      return;
    }
    requests++;
    res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({
      sql: "select id, first_name, last_name from employee where id = $1",
      params: [7],
    }));
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  try {
    await page.goto("/");
    await page.getByLabel("Email").fill(env("NURTUO_E2E_CP_EMAIL"));
    await page.getByLabel("Password", { exact: true }).fill(env("NURTUO_E2E_CP_PASSWORD"));
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByLabel("Workspace")).toBeVisible();
    await saveConnection(page, "employee-api", "http", {
      baseUrl: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, type: "bearer", token,
    });
    await saveConnection(page, "employee-db", "postgres", {
      host: "127.0.0.1", port: env("NURTUO_E2E_POSTGRES_PORT"), database: "postgres",
      user: "employee_reader", sslMode: "disable", password: env("NURTUO_E2E_POSTGRES_PASSWORD"),
    });
    const available = await page.evaluate(async () => {
      const session = await (await fetch("/v1/auth/me")).json();
      const { items } = await (await fetch(`/v1/workspaces/${session.workspaces[0].id}/connections`)).json();
      return items.map(({ id, connector }: { id: string; connector: string }) => ({ id, connector }));
    });
    expect(available).toEqual(expect.arrayContaining([
      { id: "employee-api", connector: "http" },
      { id: "employee-db", connector: "postgres" },
    ]));
    const flow = JSON.parse(readFileSync(fixture, "utf8"));
    for (const step of flow.steps) if (step.kind === "connector") delete step.config.connection;
    await page.locator('input[type="file"]').setInputFiles({
      name: "employee.flow.json", mimeType: "application/json", buffer: Buffer.from(JSON.stringify(flow)),
    });
    for (const [step, connection, operation] of [
      ["employee_request", "employee-api", "GET"],
      ["get_employee", "employee-db", "QUERY"],
    ]) {
      await page.locator(`.react-flow__node[data-id="${step}"]`).dblclick();
      const dialog = page.getByRole("dialog", { name: `Configure ${step}` });
      await expect(dialog.getByLabel("connection").locator(`option[value="${connection}"]`)).toBeAttached();
      await dialog.getByLabel("connection").selectOption(connection);
      await dialog.getByLabel("operation").selectOption(operation);
      await dialog.getByRole("button", { name: "Close" }).click();
    }
    await runFlow(page, "succeeded");
    await expect(page.getByTestId("doc-preview")).toContainText("Ada Lovelace");
    expect(requests).toBeGreaterThanOrEqual(2);

    allow = false;
    await runFlow(page, "failed");
    await expect(page.getByRole("region", { name: "Run result" }).getByText(/authentication rejected|HTTP 401/i)).toBeVisible();
    await expect(page.locator('.react-flow__node[data-id="employee_request"] .step-node')).toHaveAttribute("data-run-status", "error");
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});
