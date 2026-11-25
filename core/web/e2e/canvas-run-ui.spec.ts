// SPDX-License-Identifier: AGPL-3.0-only
// Canvas Run UI against a mocked control plane (page.route): no backend, no network.
import { expect, test, type Page, type Request } from "@playwright/test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const SAMPLE = fileURLToPath(new URL("../../../examples/flows/sftp-rebex-demo.flow.json", import.meta.url));
const PASSWORD = "p4ss-w0rd-e2e-mock";
const WS = { id: "ws-1", slug: "acme", name: "Acme", role: "editor", permissions: ["connection:read", "connection:write", "secret:write", "test_run:execute"] };

interface Mock {
  connections: Record<string, unknown>[];
  connectionPuts: unknown[];
  runBodies: unknown[];
  runResponse: { status?: number; body: unknown };
  workspace: typeof WS;
}

const step = (id: string, kind: string, status: string, failed = 0) => ({ id, kind, status, in: 1, out: failed ? 0 : 1, failed, dropped: 0 });
const OK_RUN = {
  runId: "r1", status: "succeeded", durationMs: 12,
  steps: [step("start-1", "start", "ok"), step("connector-1", "connector", "ok"), step("returndocuments-1", "returnDocuments", "ok")],
  documents: [{ id: "flow-run.out", properties: { path: "/readme.txt", filename: "readme.txt" }, size: 13, payload: Buffer.from("Hello, SFTP!\n").toString("base64") }],
};

async function mockApi(page: Page, over: Partial<Mock> = {}): Promise<Mock> {
  const m: Mock = { connections: [], connectionPuts: [], runBodies: [], runResponse: { body: OK_RUN }, workspace: WS, ...over };
  await page.route((u) => u.pathname.startsWith("/v1/"), async (route) => {
    const req = route.request();
    const { pathname } = new URL(req.url());
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (pathname === "/v1/auth/login") return json({ user: { email: "me@example.com" }, csrfToken: "csrf-1" });
    if (pathname === "/v1/auth/me") return json({ user: { email: "me@example.com" }, csrfToken: "csrf-1", workspaces: [m.workspace] });
    if (pathname.endsWith("/connections") && req.method() === "GET") return json({ items: m.connections });
    if (pathname.endsWith("/connections/host-key-scan") && req.method() === "POST") {
      return json({ keys: [{ type: "ssh-ed25519", fingerprintSha256: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }] });
    }
    const c = /\/connections\/([^/]+)$/.exec(pathname);
    if (c && req.method() === "PUT") {
      const b = req.postDataJSON() as { connector: string; settings: Record<string, unknown>; credentials?: Record<string, string | null> };
      m.connectionPuts.push(b);
      const index = m.connections.findIndex((item) => item.id === c[1]);
      const previous = m.connections[index];
      const settings = { ...(previous?.settings as object | undefined), ...b.settings };
      const secrets = { ...previous?.secrets as Record<string, string> | undefined };
      for (const [field, value] of Object.entries(b.credentials ?? {})) {
        if (value === null) delete secrets[field];
        else secrets[field] = "set";
      }
      if (b.connector === "sftp" && b.credentials?.password) settings.password = "secret://server-assigned";
      const connection = { id: c[1], connector: b.connector, settings, secrets, updatedAt: "now" };
      m.connections.splice(index < 0 ? m.connections.length : index, index < 0 ? 0 : 1, connection);
      return json(connection);
    }
    if (pathname.endsWith("/test-runs")) { m.runBodies.push(req.postDataJSON()); return json(m.runResponse.body, m.runResponse.status ?? 200); }
    return json({ error: { code: "not_found", message: "no mock" } }, 404);
  });
  return m;
}

let errors: string[] = [];
test.beforeEach(async ({ page }) => {
  errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  page.on("pageerror", (e) => errors.push(String(e)));
});

async function open(page: Page) {
  await page.goto("/");
  await expect(page.locator(".react-flow")).toBeVisible();
}
async function login(page: Page) {
  await page.getByLabel("Email").fill("me@example.com");
  await page.getByLabel("Password").fill("whatever");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByLabel("Workspace")).toBeVisible();
}
async function loadSample(page: Page) {
  await page.locator('input[type="file"]').setInputFiles({ name: "sample.flow.json", mimeType: "application/json", buffer: readFileSync(SAMPLE) });
  await expect(page.locator(".react-flow__node")).toHaveCount(3);
}
async function run(page: Page, path = "/readme.txt") {
  await page.getByRole("button", { name: "Run", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Run flow" });
  await expect(d.getByLabel("property name 0")).toHaveValue("path"); // pre-filled for SFTP GET
  await d.getByLabel("property value 0").fill(path);
  await d.getByRole("button", { name: "Run", exact: true }).click();
}
const status = (page: Page, id: string) => page.locator(`.react-flow__node[data-id="${id}"] .step-node`);

test("login and workspace picker", async ({ page }) => {
  await mockApi(page);
  await open(page);
  await expect(page.getByRole("button", { name: "Run", exact: true })).toBeDisabled();
  await login(page);
  await expect(page.getByLabel("Workspace")).toContainText("Acme");
  await expect(page.getByRole("button", { name: "Connections" })).toBeEnabled();
  expect(errors).toEqual([]);
});

test("offline canvas is unchanged and Run is disabled", async ({ page }) => {
  await page.route((u) => u.pathname.startsWith("/v1/"), (r) => r.abort());
  await open(page);
  await page.getByLabel("Email").fill("a");
  await page.getByLabel("Password").fill("b");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByText(/Offline/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Run", exact: true })).toBeDisabled();
  await loadSample(page); // local editing keeps working
  await page.locator(".react-flow__node[data-id='connector-1']").dblclick();
  await expect(page.getByRole("dialog").locator("label.field", { hasText: /^connection/ }).locator("input")).toHaveValue("rebex-demo");
});

test("create connection sends the password in a single write-only PUT", async ({ page }) => {
  const m = await mockApi(page);
  const sent: { url: string; body: string }[] = [];
  page.on("request", (r: Request) => { if (r.url().includes("/v1/") && r.method() !== "GET") sent.push({ url: new URL(r.url()).pathname, body: r.postData() ?? "" }); });
  await open(page);
  await login(page);
  await page.getByRole("button", { name: "Connections" }).click();
  const d = page.getByRole("dialog", { name: "Connections" });
  await expect(d.getByLabel("password")).toHaveAttribute("type", "password");
  await d.getByLabel("Connection id").fill("rebex-demo");
  await d.getByLabel("host", { exact: true }).fill("test.rebex.net");
  await d.getByLabel("user").fill("demo");
  await d.getByLabel("password").fill(PASSWORD);
  await d.getByLabel("Host key").fill("SHA256:abc123");
  await expect(d.getByText(/insecure/i)).toHaveCount(0);
  await d.getByRole("button", { name: "Save connection" }).click();
  await expect(d.locator('[data-connection="rebex-demo"]')).toBeVisible();

  expect(m.connectionPuts).toHaveLength(1);
  const withPassword = sent.filter((s) => s.body.includes(PASSWORD));
  expect(withPassword.map((s) => s.url)).toEqual(["/v1/workspaces/ws-1/connections/rebex-demo"]);
  expect(JSON.parse(withPassword[0]!.body)).toMatchObject({
    connector: "sftp",
    settings: { hostKeyPolicy: "fingerprint", hostKeyFingerprint: "SHA256:abc123" },
    credentials: { password: PASSWORD },
  });
  expect(JSON.parse(withPassword[0]!.body).settings).not.toHaveProperty("password");
  expect(await d.getByLabel("password").inputValue()).toBe(""); // never pre-filled
  expect(errors).toEqual([]);
});

test("an API-created connection keeps server settings and credential when edited in canvas", async ({ page }) => {
  const original = {
    host: "sftp.example.com", user: "demo", port: 22,
    hostKeyPolicy: "fingerprint", hostKeyFingerprint: "SHA256:abc123",
    password: "secret://server-assigned-for-api", timeoutMs: 9000, maxListEntries: 42,
  };
  const m = await mockApi(page, { connections: [{ id: "from-api", connector: "sftp", settings: original, secrets: { password: "set" } }] });
  await open(page);
  await login(page);
  await page.getByRole("button", { name: "Connections" }).click();
  const dialog = page.getByRole("dialog", { name: "Connections" });
  await dialog.locator('[data-connection="from-api"]').click();
  await expect(dialog.getByLabel("password")).toHaveValue("");
  await dialog.getByLabel("rootPath").fill("/reports");
  await dialog.getByRole("button", { name: "Save connection" }).click();
  await expect.poll(() => m.connectionPuts.length).toBe(1);
  expect(m.connectionPuts[0]).not.toHaveProperty("credentials");
  expect(m.connectionPuts[0]).not.toHaveProperty("settings.password");
  expect(m.connections[0]?.settings).toEqual({ ...original, rootPath: "/reports" });
});

test("connector config offers workspace connections", async ({ page }) => {
  await mockApi(page, { connections: [{ id: "rebex-demo", connector: "sftp", settings: { host: "h" }, secrets: { password: "set" } }, { id: "other", connector: "http", settings: {}, secrets: {} }] });
  await open(page);
  await login(page);
  await loadSample(page);
  await page.locator(".react-flow__node[data-id='connector-1']").dblclick();
  const sel = page.getByRole("dialog").getByLabel("connection");
  await expect(sel.locator("option")).toHaveText(["", "rebex-demo"]);
});

test("run success colours three nodes and shows the text", async ({ page }) => {
  const m = await mockApi(page);
  await open(page);
  await login(page);
  await loadSample(page);
  await run(page);
  await expect(page.getByTestId("run-status")).toHaveText("succeeded");
  for (const id of ["start-1", "connector-1", "returndocuments-1"]) {
    await expect(status(page, id)).toHaveAttribute("data-run-status", "ok");
    await expect(status(page, id)).toHaveClass(/run-ok/);
    await expect(status(page, id)).toContainText("ok");
  }
  await expect(page.getByTestId("doc-preview")).toHaveText("Hello, SFTP!");
  expect(m.runBodies[0]).toMatchObject({ input: { properties: { path: "/readme.txt" } }, flow: { id: "sftp-rebex-demo" } });
  expect(JSON.stringify(m.runBodies[0])).not.toContain("layout");
  expect(errors).toEqual([]);
});

test("run failure colours the connector and shows the message", async ({ page }) => {
  await mockApi(page, { runResponse: { body: {
    runId: "r2", status: "failed", durationMs: 5,
    steps: [step("start-1", "start", "not_run"), step("connector-1", "connector", "error"), step("returndocuments-1", "returnDocuments", "not_run")],
    error: { code: "execution", message: 'connection "rebex-demo" (sftp): test failed: ssh connect: host key mismatch: got SHA256:a, want SHA256:b' },
  } } });
  await open(page);
  await login(page);
  await loadSample(page);
  await run(page);
  await expect(status(page, "connector-1")).toHaveAttribute("data-run-status", "error");
  await expect(status(page, "returndocuments-1")).toHaveAttribute("data-run-status", "not_run");
  await expect(page.getByText(/host key mismatch/).first()).toBeVisible();
  await expect(page.getByText(/server's key changed/)).toBeVisible();
});

test("API errors are explained (secret missing)", async ({ page }) => {
  await mockApi(page, { runResponse: { status: 409, body: { error: { code: "secret_missing", message: 'secret "rebex-demo-password" of connection "rebex-demo" is not set.' } } } });
  await open(page);
  await login(page);
  await loadSample(page);
  await run(page);
  await expect(page.getByRole("alert")).toContainText("Open Connections");
});

test("editing the flow clears colours and results", async ({ page }) => {
  await mockApi(page);
  await open(page);
  await login(page);
  await loadSample(page);
  await run(page);
  await expect(status(page, "connector-1")).toHaveAttribute("data-run-status", "ok");
  await page.locator(".react-flow__node[data-id='start-1']").dblclick();
  const d = page.getByRole("dialog");
  await d.locator("label.field", { hasText: /^label/ }).locator("input").fill("renamed");
  await d.getByRole("button", { name: "Close" }).click();
  await expect(page.locator("[data-run-status]")).toHaveCount(0);
  await expect(page.getByTestId("run-status")).toHaveCount(0);
});

test("binary output shows size, not content", async ({ page }) => {
  await mockApi(page, { runResponse: { body: { ...OK_RUN, documents: [{ id: "b", properties: {}, size: 3, payload: Buffer.from([0xff, 0xfe, 0x00]).toString("base64") }] } } });
  await open(page);
  await login(page);
  await loadSample(page);
  await run(page);
  await expect(page.getByTestId("doc-preview")).toHaveText("binary, 3 bytes");
});

test("a role without test_run:execute cannot run", async ({ page }) => {
  await mockApi(page, { workspace: { ...WS, role: "viewer", permissions: ["connection:read"] } });
  await open(page);
  await login(page);
  await loadSample(page);
  const b = page.getByRole("button", { name: "Run", exact: true });
  await expect(b).toBeDisabled();
  await expect(b).toHaveAttribute("title", /cannot run/);
});

test("TOFU stores nothing until Trust", async ({ page }) => {
  const m = await mockApi(page);
  await open(page);
  await login(page);
  await page.getByRole("button", { name: "Connections" }).click();
  const d = page.getByRole("dialog", { name: "Connections" });
  await d.getByLabel("Connection id").fill("rebex-demo");
  await d.getByLabel("host", { exact: true }).fill("test.rebex.net");
  await d.getByLabel("user", { exact: true }).fill("demo");
  await d.getByLabel("password").fill(PASSWORD);
  await d.getByRole("button", { name: "Fetch host key" }).click();
  await expect(d.getByRole("button", { name: "Trust this key" })).toBeVisible();
  await expect(d.getByLabel("Host key")).toHaveValue("");
  await d.getByRole("button", { name: "Save connection" }).click();
  await expect(d.getByRole("alert")).toContainText("A host key is required");
  expect(m.connections).toEqual([]);
  expect(m.connectionPuts).toEqual([]);
  await d.getByRole("button", { name: "Trust this key" }).click();
  await expect(d.getByLabel("Host key")).toHaveValue("SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
  await d.getByRole("button", { name: "Save connection" }).click();
  await expect(d.locator('[data-connection="rebex-demo"]')).toBeVisible();
  expect(m.connections[0]?.settings.hostKeyPolicy).toBe("fingerprint");
  expect(errors).toEqual([]);
});

test("editing HTTP authentication clears stale credentials and clears the token after save", async ({ page }) => {
  const m = await mockApi(page, { connections: [{
    id: "web", connector: "http", settings: { baseUrl: "https://example.com", auth: { type: "basic", username: "demo" } },
    secrets: { password: "set" },
  }] });
  await open(page);
  await login(page);
  await page.getByRole("button", { name: "Connections" }).click();
  const d = page.getByRole("dialog", { name: "Connections" });
  await d.locator('[data-connection="web"]').click();
  await expect(d.getByLabel("baseUrl")).toHaveValue("https://example.com");
  await expect(d.getByLabel("username")).toHaveValue("demo");
  await expect(d.getByLabel("password")).toHaveValue("");
  await d.getByLabel("password").fill("draft-password");
  await d.getByLabel("type", { exact: true }).selectOption("bearer");
  await d.getByLabel("token").fill("draft-token");
  await d.getByLabel("type", { exact: true }).selectOption("basic");
  await expect(d.getByLabel("password")).toHaveValue("");
  await d.getByLabel("type", { exact: true }).selectOption("bearer");
  await expect(d.getByLabel("token")).toHaveValue("");
  await d.getByLabel("token").fill("replacement-token");
  await d.getByRole("button", { name: "Save connection" }).click();
  await expect(d.locator('[data-connection="web"]')).toContainText("token: set");
  await expect(d.getByLabel("token")).toHaveValue("");
  expect(m.connectionPuts).toEqual([{
    connector: "http", settings: { baseUrl: "https://example.com", auth: { type: "bearer" } },
    credentials: { token: "replacement-token", password: null },
  }]);
});

test("editing PostgreSQL prefills settings and explicitly removes the stored password", async ({ page }) => {
  const settings = { host: "db.example.com", port: 5433, database: "catalog", user: "reader", sslMode: "verify-full" };
  const m = await mockApi(page, { connections: [{ id: "db", connector: "postgres", settings, secrets: { password: "set" } }] });
  await open(page);
  await login(page);
  await page.getByRole("button", { name: "Connections" }).click();
  const d = page.getByRole("dialog", { name: "Connections" });
  await d.locator('[data-connection="db"]').click();
  await expect(d.getByLabel("host", { exact: true })).toHaveValue(settings.host);
  await expect(d.getByLabel("port", { exact: true })).toHaveValue("5433");
  await expect(d.getByLabel("database")).toHaveValue(settings.database);
  await expect(d.getByLabel("sslMode")).toHaveValue(settings.sslMode);
  await expect(d.getByLabel("password", { exact: true })).toHaveValue("");
  const removePassword = d.getByLabel("Remove stored password");
  await removePassword.check();
  await expect(d.getByLabel("password", { exact: true })).toBeDisabled();
  await d.getByRole("button", { name: "Save connection" }).click();
  await expect(removePassword).toHaveCount(0);
  expect(m.connectionPuts).toEqual([{ connector: "postgres", settings, credentials: { password: null } }]);
});

async function openLegacyHttpStep(page: Page) {
  const flow = JSON.parse(readFileSync(SAMPLE, "utf8"));
  flow.steps[1].config = { connector: "http", operation: "POST", objectType: "legacy" };
  await page.locator('input[type="file"]').setInputFiles({
    name: "legacy.flow.json", mimeType: "application/json", buffer: Buffer.from(JSON.stringify(flow)),
  });
  await page.locator(".react-flow__node[data-id='connector-1']").dblclick();
  return page.getByRole("dialog", { name: "Configure connector-1" });
}

test("workspace binding configures HTTP GET and PostgreSQL QUERY and can be cleared", async ({ page }) => {
  await mockApi(page, { connections: [
    { id: "web", connector: "http", settings: {}, secrets: {} },
    { id: "db", connector: "postgres", settings: {}, secrets: {} },
  ] });
  await open(page);
  await login(page);
  const d = await openLegacyHttpStep(page);
  await expect(d.getByRole("textbox", { name: "operation" })).toHaveValue("POST");
  await expect(d.getByRole("textbox", { name: "objectType" })).toHaveValue("legacy");

  for (const [id, connector, operation] of [["web", "http", "GET"], ["db", "postgres", "QUERY"]] as const) {
    await d.getByLabel("connection", { exact: true }).selectOption(id);
    await expect(d.getByRole("combobox", { name: "operation" })).toHaveValue(operation);
    await expect(d.getByRole("textbox", { name: "objectType" })).toHaveCount(0);
    await d.getByLabel("connection", { exact: true }).selectOption("");
    await expect(d.getByRole("textbox", { name: "connector", exact: false })).toHaveValue(connector);
    await expect(d.getByRole("textbox", { name: "operation" })).toHaveValue(operation);
    await expect(d.getByRole("textbox", { name: "objectType" })).toHaveValue("");
  }
});

test("an empty workspace connection list preserves the legacy connector editor", async ({ page }) => {
  await mockApi(page);
  await open(page);
  await login(page);
  const d = await openLegacyHttpStep(page);
  await expect(d.getByRole("textbox", { name: "operation" })).toHaveValue("POST");
  await expect(d.getByRole("textbox", { name: "objectType" })).toHaveValue("legacy");
  await expect(d.getByRole("textbox", { name: "connection" })).toBeEditable();
  const connector = d.getByRole("textbox", { name: "connector" });
  await connector.fill("sftp");
  await connector.press("Tab");
  await expect(connector).toHaveValue("sftp");
  await d.getByRole("button", { name: "Close" }).click();
  await page.locator(".react-flow__node[data-id='connector-1']").dblclick();
  await expect(connector).toHaveValue("sftp");
});
