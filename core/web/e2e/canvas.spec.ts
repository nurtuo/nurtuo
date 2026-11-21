// SPDX-License-Identifier: AGPL-3.0-only
import { expect, test, type Locator, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const EXAMPLE = fileURLToPath(new URL("../../../examples/flows/get-employee.flow.json", import.meta.url));

let errors: string[] = [];

test.beforeEach(async ({ page }) => {
  errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  page.on("pageerror", (e) => errors.push(String(e)));
  await page.goto("/");
  await expect(page.locator(".react-flow")).toBeVisible();
});

const node = (page: Page, id: string) => page.locator(`.react-flow__node[data-id="${id}"]`);
const nodes = (page: Page) => page.locator(".react-flow__node");
const edges = (page: Page) => page.locator(".react-flow__edge");

async function addStep(page: Page, label: string, x: number, y: number) {
  const pane = page.locator(".react-flow__pane");
  await page.locator(".palette-item", { hasText: new RegExp(`^${label}$`) }).dragTo(pane, { targetPosition: { x, y } });
}

async function dragHandle(page: Page, src: Locator, dst: Locator) {
  const a = (await src.boundingBox())!;
  const b = (await dst.boundingBox())!;
  await page.mouse.move(a.x + a.width / 2, a.y + a.height / 2);
  await page.mouse.down();
  await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2, { steps: 12 });
  await page.mouse.up();
}

async function connect(page: Page, from: string, port: string, to: string) {
  const src = node(page, from).locator(`.react-flow__handle.source[data-handleid="${port}"]`);
  const dst = node(page, to).locator(".react-flow__handle.target");
  await dragHandle(page, src, dst);
}

async function load(page: Page, path = EXAMPLE) {
  await page.locator('input[type="file"]').setInputFiles(path);
  await expect(nodes(page).first()).toBeVisible();
}

async function saveFlow(page: Page) {
  const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Save flow" }).click()]);
  return JSON.parse(readFileSync((await dl.path())!, "utf8"));
}

test("app loads without console errors", async ({ page }) => {
  await expect(page.locator(".palette-item").first()).toBeVisible();
  await page.waitForTimeout(300);
  expect(errors).toEqual([]);
});

test("drag step from palette onto canvas", async ({ page }) => {
  await addStep(page, "Start", 300, 200);
  await expect(nodes(page)).toHaveCount(1);
  await expect(node(page, "start-1")).toHaveClass(/selected/);
  expect(errors).toEqual([]);
});

test("connect two nodes via handles", async ({ page }) => {
  await addStep(page, "Start", 300, 100);
  await addStep(page, "Stop", 300, 350);
  await connect(page, "start-1", "default", "stop-1");
  await expect(edges(page)).toHaveCount(1);
});

test("invalid connections are rejected", async ({ page }) => {
  await addStep(page, "Start", 150, 100);
  await addStep(page, "Map", 150, 250);
  await addStep(page, "Stop", 450, 400);
  await connect(page, "start-1", "default", "map-1");
  await expect(edges(page)).toHaveCount(1);
  // self loop
  await connect(page, "map-1", "default", "map-1");
  await expect(edges(page)).toHaveCount(1);
  // into Start (Start has no target handle; dropping on the node body must not connect either)
  await dragHandle(page, node(page, "map-1").locator(".react-flow__handle.source"), node(page, "start-1"));
  await expect(edges(page)).toHaveCount(1);
  // occupied port: start:default already used
  await connect(page, "start-1", "default", "stop-1");
  await expect(edges(page)).toHaveCount(1);
  // a free port still connects
  await connect(page, "map-1", "default", "stop-1");
  await expect(edges(page)).toHaveCount(2);
});

test("select and Delete removes node and its edges", async ({ page }) => {
  await load(page);
  await expect(nodes(page)).toHaveCount(6);
  await node(page, "found").click();
  await page.keyboard.press("Delete");
  await expect(nodes(page)).toHaveCount(5);
  await expect(edges(page)).toHaveCount(2);
});

test("undo/redo via buttons and shortcuts", async ({ page }) => {
  await addStep(page, "Start", 300, 200);
  await expect(nodes(page)).toHaveCount(1);
  await page.getByRole("button", { name: "Undo" }).click();
  await expect(nodes(page)).toHaveCount(0);
  await page.getByRole("button", { name: "Redo" }).click();
  await expect(nodes(page)).toHaveCount(1);
  await page.locator(".react-flow__pane").click({ position: { x: 600, y: 500 } });
  await page.keyboard.press("Control+z");
  await expect(nodes(page)).toHaveCount(0);
  await page.keyboard.press("Control+Shift+z");
  await expect(nodes(page)).toHaveCount(1);
});

test("config dialog: edit, undo restores model and form field", async ({ page }) => {
  await load(page);
  await node(page, "found").dblclick();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  const field = (name: string) => dialog.locator("label.field", { hasText: new RegExp(`^${name}`) }).locator("input, textarea").first();
  const cel = field("cel");
  await expect(cel).toHaveValue("doc.id != ''");
  await cel.fill("doc.id == 'x'");
  await field("label").focus(); // blur commits
  await expect(cel).toHaveValue("doc.id == 'x'");
  // The modal overlay covers the toolbar, so undo/redo while it is open goes through the keyboard.
  await dialog.locator("h3").click(); // move focus off the inputs so the shortcut is not swallowed
  await page.keyboard.press("Control+z");
  await expect(cel).toHaveValue("doc.id != ''");
  await page.keyboard.press("Control+Shift+z");
  await expect(cel).toHaveValue("doc.id == 'x'");
  await page.keyboard.press("Control+z");
  await expect(cel).toHaveValue("doc.id != ''");
  await dialog.getByRole("button", { name: "Close" }).click();
  expect((await saveFlow(page)).steps.find((s: { id: string }) => s.id === "found").config.cel).toBe("doc.id != ''");
  await page.getByRole("button", { name: "Redo" }).click();
  expect((await saveFlow(page)).steps.find((s: { id: string }) => s.id === "found").config.cel).toBe("doc.id == 'x'");
});

test("diagnostics list issues and clicking selects the node", async ({ page }) => {
  await addStep(page, "Map", 300, 200);
  const items = page.locator(".diagnostics li");
  await expect(items.filter({ hasText: "map-1" }).first()).toBeVisible();
  await page.locator(".react-flow__pane").click({ position: { x: 700, y: 500 } });
  await expect(node(page, "map-1")).not.toHaveClass(/selected/);
  await items.filter({ hasText: "map-1" }).first().click();
  await expect(node(page, "map-1")).toHaveClass(/selected/);
  await expect(page.locator(".panel")).toContainText("map-1");
});

test("ctrl-click deselect clears selection and closes panel", async ({ page }) => {
  await load(page);
  await node(page, "found").click();
  await expect(page.locator(".panel")).toContainText("Configure");
  await node(page, "found").click({ modifiers: ["Control"] });
  await expect(node(page, "found")).not.toHaveClass(/selected/);
  await expect(page.locator(".panel")).not.toContainText("Configure");
  await expect(page.locator(".panel")).toContainText("Select a step");
});

test("load example renders all nodes and edges; save round-trips identically", async ({ page }) => {
  await load(page);
  await expect(nodes(page)).toHaveCount(6);
  await expect(edges(page)).toHaveCount(5);
  expect(await saveFlow(page)).toEqual(JSON.parse(readFileSync(EXAMPLE, "utf8")));
  expect(errors).toEqual([]);
});
