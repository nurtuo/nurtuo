#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// `nurtuo-mcp` executable entry (package.json "bin"). Runs stdio by default, `--transport http` for streamable HTTP.
import { main } from "./cli.js";

main(process.argv.slice(2)).catch((e: unknown) => {
  process.stderr.write(`nurtuo-mcp: fatal: ${e instanceof Error ? e.message : String(e)}\n`);
  process.exit(1);
});
