// SPDX-License-Identifier: AGPL-3.0-only
import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./test-results",
  fullyParallel: true,
  retries: 0,
  // CI also writes an HTML report (core/web/playwright-report), uploaded as an artifact when the job fails.
  reporter: process.env["CI"] ? [["list"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL: "http://127.0.0.1:5199",
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 800 } } }],
  webServer: {
    command: "pnpm --filter \"@nurtuo/web^...\" run build && pnpm exec vite --host 127.0.0.1 --port 5199 --strictPort",
    url: "http://127.0.0.1:5199",
    reuseExistingServer: !process.env["CI"],
    timeout: 240_000,
  },
});
