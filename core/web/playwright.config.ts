// SPDX-License-Identifier: AGPL-3.0-only
import { defineConfig, devices } from "@playwright/test";
import { randomBytes } from "node:crypto";

// Full canvas-run e2e (e2e/canvas-run.spec.ts): also start the control plane, only when asked for.
// Generated once in the main process (workers inherit the environment), never printed.
const rebex = process.env["NURTUO_E2E_REBEX"] === "1";
const canvasRun = process.env["NURTUO_CANVAS_RUN_E2E"] === "1" || rebex;
const CP_PORT = process.env["NURTUO_E2E_CP_PORT"] ?? "18081";
process.env["NURTUO_E2E_CP_EMAIL"] ??= "e2e@example.com";
process.env["NURTUO_E2E_CP_PASSWORD"] ??= randomBytes(16).toString("hex");
const CP_URL = `http://127.0.0.1:${CP_PORT}`;

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./test-results",
  fullyParallel: true,
  retries: 0,
  // CI also writes an HTML report (core/web/playwright-report), uploaded as an artifact when the job fails.
  reporter: process.env["CI"] ? [["list"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL: "http://127.0.0.1:5199",
    screenshot: rebex ? "off" : "only-on-failure",
    trace: rebex ? "off" : "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 800 } } }],
  webServer: [
    {
      command: "pnpm --filter \"@nurtuo/web^...\" run build && pnpm exec vite --host 127.0.0.1 --port 5199 --strictPort",
      url: "http://127.0.0.1:5199",
      reuseExistingServer: !canvasRun && !process.env["CI"],
      timeout: 240_000,
      env: canvasRun ? { NURTUO_CP_URL: CP_URL } : {},
    },
    ...(canvasRun
      ? [{
          command: "node ../control-plane/dist/main.js",
          url: `${CP_URL}/v1/auth/me`,
          // 401 means the server is up (the check accepts any status below 500).
          ignoreHTTPSErrors: true,
          reuseExistingServer: false,
          timeout: 60_000,
          env: {
            PORT: CP_PORT,
            NURTUO_AUTH_SECRET: randomBytes(32).toString("hex"),
            NURTUO_BOOTSTRAP_EMAIL: process.env["NURTUO_E2E_CP_EMAIL"]!,
            NURTUO_BOOTSTRAP_PASSWORD: process.env["NURTUO_E2E_CP_PASSWORD"]!,
            NURTUO_TEST_RUN_ALLOW_PRIVATE: "1",
            ...(process.env["NURTUO_FLOW_RUN_BIN"] ? { NURTUO_FLOW_RUN_BIN: process.env["NURTUO_FLOW_RUN_BIN"] } : {}),
          },
        }]
      : []),
  ],
});
