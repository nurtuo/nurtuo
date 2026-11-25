// SPDX-License-Identifier: AGPL-3.0-only
// Copies connector connection schemas into dist so the published package (dist, migrations, openapi.yaml) can validate connections.
import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const out = join(root, "dist", "connections", "schemas");
mkdirSync(out, { recursive: true });
copyFileSync(join(root, "..", "..", "connectors", "sftp", "schemas", "connection.json"), join(out, "sftp.connection.json"));
