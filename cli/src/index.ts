#!/usr/bin/env node
// SPDX-License-Identifier: Apache-2.0
import { run } from "./cli.js";

async function readStdin(): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const c of process.stdin) chunks.push(c as Buffer);
  return Buffer.concat(chunks).toString("utf8");
}

const code = await run(process.argv.slice(2), {
  stdout: (s) => void process.stdout.write(s),
  stderr: (s) => void process.stderr.write(s),
  env: process.env,
  readStdin,
});
// Let buffered output flush instead of calling process.exit().
process.exitCode = code;
