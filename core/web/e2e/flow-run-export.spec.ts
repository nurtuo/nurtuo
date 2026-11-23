// SPDX-License-Identifier: AGPL-3.0-only
// Builds the SFTP-download flow in the canvas, exports it with "Save flow" and checks that the
// export is what `nurtuo flow run` accepts. The binary run is gated on NURTUO_FLOW_RUN_BIN (a built
// `nurtuo`), NURTUO_FLOW_RUN_CONN (a connection file with id private-sftp), NURTUO_FLOW_RUN_KNOWN_HOSTS
// and NURTUO_FLOW_RUN_EXPECT (the exact content of /downloads/version.txt on that server); secrets
// come from the NURTUO_SECRET_* environment as for the CLI itself.
import { expect, test, type Page } from "@playwright/test";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const EXAMPLE = fileURLToPath(new URL("../../../examples/flows/sftp-download.flow.json", import.meta.url));

const node = (page: Page, id: string) => page.locator(`.react-flow__node[data-id="${id}"]`);

async function addStep(page: Page, label: string, x: number, y: number) {
  await page.locator(".palette-item", { hasText: new RegExp(`^${label}$`) }).dragTo(page.locator(".react-flow__pane"), { targetPosition: { x, y } });
}

async function connect(page: Page, from: string, to: string) {
  const a = (await node(page, from).locator('.react-flow__handle.source[data-handleid="default"]').boundingBox())!;
  const b = (await node(page, to).locator(".react-flow__handle.target").boundingBox())!;
  await page.mouse.move(a.x + a.width / 2, a.y + a.height / 2);
  await page.mouse.down();
  await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2, { steps: 12 });
  await page.mouse.up();
}

test("canvas-built SFTP download flow exports a valid input for `nurtuo flow run`", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".react-flow")).toBeVisible();
  await addStep(page, "Start", 100, 100);
  await addStep(page, "Connector", 100, 250);
  await addStep(page, "Return Documents", 100, 400);
  await connect(page, "start-1", "connector-1");
  await connect(page, "connector-1", "returndocuments-1");

  await node(page, "connector-1").dblclick();
  const dialog = page.getByRole("dialog");
  const field = (name: string) => dialog.locator("label.field", { hasText: new RegExp(`^${name}`) }).locator("input, textarea").first();
  for (const [name, value] of [["connector", "sftp"], ["operation", "GET"], ["connection", "private-sftp"]] as const) {
    await field(name).fill(value);
    await field("label").focus(); // blur commits
  }
  await dialog.getByRole("button", { name: "Close" }).click();

  const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Save flow" }).click()]);
  const exported = JSON.parse(readFileSync((await dl.path())!, "utf8"));

  // Same shape as the checked-in example that the CLI docs and the container e2e use.
  const example = JSON.parse(readFileSync(EXAMPLE, "utf8"));
  const shape = (f: { steps: { kind: string; config: unknown }[] }) => f.steps.map((s) => ({ kind: s.kind, config: s.config }));
  expect(exported.schema).toBe("nurtuo.flow/v1");
  expect(shape(exported)).toEqual(shape(example));
  expect(Object.values(exported.edges)).toHaveLength(2);

  const bin = process.env["NURTUO_FLOW_RUN_BIN"];
  test.skip(!bin, "set NURTUO_FLOW_RUN_BIN (+ _CONN, _KNOWN_HOSTS, _EXPECT) to run the exported flow with the real binary");
  const file = join(mkdtempSync(join(tmpdir(), "flow-run-export-")), "exported.flow.json");
  writeFileSync(file, JSON.stringify(exported));
  const r = spawnSync(bin!, ["flow", "run", file, "--connection", process.env["NURTUO_FLOW_RUN_CONN"]!,
    "--known-hosts", process.env["NURTUO_FLOW_RUN_KNOWN_HOSTS"]!, "--set", "path=/downloads/version.txt"], { encoding: "utf8" });
  expect({ code: r.status, stderr: r.stderr }).toEqual({ code: 0, stderr: "" });
  expect(r.stdout).toBe(process.env["NURTUO_FLOW_RUN_EXPECT"]);
});
