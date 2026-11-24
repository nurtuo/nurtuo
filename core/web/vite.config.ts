// SPDX-License-Identifier: AGPL-3.0-only
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  // The canvas talks to the control plane same-origin (session cookie); see the canvas-run spec D9.
  server: { proxy: { "/v1": process.env["NURTUO_CP_URL"] ?? "http://127.0.0.1:8080" } },
  test: { include: ["src/**/*.test.ts"], environment: "node" },
});
