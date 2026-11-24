// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { secretFields } from "./validate.js";

test("the dist copy of connection.json equals connectors/sftp/schemas/connection.json", () => {
  const src = readFileSync(fileURLToPath(new URL("../../../../connectors/sftp/schemas/connection.json", import.meta.url)), "utf8");
  const copy = readFileSync(fileURLToPath(new URL("./schemas/sftp.connection.json", import.meta.url)), "utf8");
  assert.equal(copy, src);
});

test("secret fields come from the schema", () => { assert.deepEqual(secretFields("sftp").sort(), ["passphrase", "password", "privateKey"]); });
