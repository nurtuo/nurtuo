// SPDX-License-Identifier: AGPL-3.0-only
// Test launcher: REST API + runtime gateway (mTLS) over an in-memory repository.
// Env: CERT_DIR (required), GATEWAY_PORT / REST_PORT (default 0 = random), CP_DIST (built dist dir),
// IDLE_TIMEOUT_MS. Prints one JSON line {"restPort":n,"gatewayPort":n} once both are listening.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const dist = process.env.CP_DIST ?? fileURLToPath(new URL("../../../../control-plane/dist/", import.meta.url));
const { MemoryRepository, Gateway, createGatewayServer, createHttpServer } = await import(pathToFileURL(join(dist, "index.js")).href);
const certDir = process.env.CERT_DIR;
if (!certDir) throw new Error("CERT_DIR is required");
const f = (n) => readFileSync(join(certDir, n));

const repo = new MemoryRepository();
const gw = new Gateway(repo, { log: (m) => console.error(`gateway: ${m}`), idleTimeoutMs: Number(process.env.IDLE_TIMEOUT_MS ?? 0) });
gw.startOutboxPublisher(100);
const gwServer = createGatewayServer(gw, { cert: f("server.pem"), key: f("server.key"), ca: f("ca.pem") });
const rest = createHttpServer(repo, { auth: "disabled" }); // test launcher only: auth is covered in control-plane tests
const listen = (s, port) => new Promise((res, rej) => { s.once("error", rej); s.listen(port, "127.0.0.1", () => res(s.address().port)); });
const gatewayPort = await listen(gwServer, Number(process.env.GATEWAY_PORT ?? 0));
const restPort = await listen(rest, Number(process.env.REST_PORT ?? 0));
console.log(JSON.stringify({ restPort, gatewayPort }));
