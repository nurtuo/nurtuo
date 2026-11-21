#!/usr/bin/env node
// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import type { Flow } from "@nurtuo/flow-schema";
import { validateFlow } from "@nurtuo/flow-validate";

const [cmd, file] = process.argv.slice(2);
if (cmd === "validate" && file) {
  const issues = validateFlow(JSON.parse(readFileSync(file, "utf8")) as Flow);
  for (const i of issues) console.log(`${i.severity}\t${i.code}\t${i.message}`);
  process.exit(issues.some((i) => i.severity === "error") ? 1 : 0);
} else {
  console.log("Usage: nurtuoctl validate <flow.json>");
  process.exit(cmd ? 2 : 0);
}
