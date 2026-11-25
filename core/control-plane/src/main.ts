// SPDX-License-Identifier: AGPL-3.0-only
// Executable entry: DATABASE_URL set -> Postgres (requires the `pg` package), else in-memory.
//
// Auth is mandatory. Required: NURTUO_AUTH_SECRET (>=32 bytes; with DATABASE_URL it must be stable
// across restarts, without it a random per-process secret is used and sessions die on restart).
// First run: set NURTUO_BOOTSTRAP_EMAIL + NURTUO_BOOTSTRAP_PASSWORD to create the first owner.
// Workspace creation: NURTUO_INSTANCE_ADMINS (comma-separated emails) plus NURTUO_BOOTSTRAP_EMAIL are
// instance admins; NURTUO_ALLOW_WORKSPACE_CREATE=admins (default) or any. See docs/auth.md.
// Escape hatch for local development only: NURTUO_INSECURE_NO_AUTH=1 serves every route unauthenticated.
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";
import { createHttpServer, type HandlerOptions } from "./api.js";
import { AuthService, InstanceConfigError, MemoryAuthRepository, PgAuthRepository, createAuthHook, parseInstanceConfig, type AuthRepository, type InstanceConfig } from "./auth/index.js";
import { MemoryRepository } from "./memory-repository.js";
import { PgRepository, migrate, type Db } from "./pg-repository.js";
import type { Repository } from "./repository.js";

const insecure = process.env.NURTUO_INSECURE_NO_AUTH === "1";

// Instance config is parsed before any database work; without auth POST /v1/workspaces is unreachable.
let instance: InstanceConfig | undefined;
if (!insecure) {
  try { instance = parseInstanceConfig(process.env); }
  catch (e) { if (e instanceof InstanceConfigError) { console.error(e.message); process.exit(1); } throw e; }
  if (instance.workspaceCreate === "any") console.warn("NURTUO_ALLOW_WORKSPACE_CREATE=any: every session user can create a workspace and becomes its owner; control-plane data is not partitioned by workspace yet (#55)");
  else if (instance.admins.size === 0) console.warn("no instance admins configured (NURTUO_INSTANCE_ADMINS, NURTUO_BOOTSTRAP_EMAIL): POST /v1/workspaces is denied for everyone");
  else console.log(`instance admins: ${instance.admins.size} configured`);
}

let repo: Repository = new MemoryRepository();
let authRepo: AuthRepository = new MemoryAuthRepository();
if (process.env.DATABASE_URL) {
  const mod = "pg"; // resolved at runtime so the package is optional for tests
  const pg = (await import(mod)) as { default: { Pool: new (o: { connectionString: string }) => Db } };
  const pool = new pg.default.Pool({ connectionString: process.env.DATABASE_URL });
  await migrate(pool, fileURLToPath(new URL("../migrations", import.meta.url)));
  repo = new PgRepository(pool);
  authRepo = new PgAuthRepository(pool);
}

let opts: HandlerOptions;
if (insecure) {
  console.warn("WARNING: NURTUO_INSECURE_NO_AUTH=1, authentication and authorization are DISABLED. Development only.");
  opts = { auth: "disabled" };
} else {
  let secret = process.env.NURTUO_AUTH_SECRET ? Buffer.from(process.env.NURTUO_AUTH_SECRET, "utf8") : undefined;
  if (!secret) {
    if (process.env.DATABASE_URL) { console.error("NURTUO_AUTH_SECRET is required when DATABASE_URL is set (min 32 chars)."); process.exit(1); }
    secret = randomBytes(48);
    console.warn("NURTUO_AUTH_SECRET not set: using a random per-process secret (sessions are lost on restart).");
  }
  if (secret.length < 32) { console.error("NURTUO_AUTH_SECRET must be at least 32 bytes."); process.exit(1); }
  const service = new AuthService(authRepo, { secret, instance });
  if (process.env.NURTUO_BOOTSTRAP_EMAIL && process.env.NURTUO_BOOTSTRAP_PASSWORD) {
    await service.bootstrap(process.env.NURTUO_BOOTSTRAP_EMAIL, process.env.NURTUO_BOOTSTRAP_PASSWORD);
    console.log(`bootstrap owner ensured: ${process.env.NURTUO_BOOTSTRAP_EMAIL}`);
  }
  opts = { auth: createAuthHook({ service, trustProxy: process.env.NURTUO_TRUST_PROXY === "1", onAuditError: (e) => console.error("audit write failed", e) }) };
}
const port = Number(process.env.PORT ?? 8080);
createHttpServer(repo, opts).listen(port, () => console.log(`nurtuo-control-plane listening on :${port}`));

// Runtime gateway (mTLS) when certificates are configured.
const { GATEWAY_CERT, GATEWAY_KEY, GATEWAY_CA } = process.env;
if (GATEWAY_CERT && GATEWAY_KEY && GATEWAY_CA) {
  const { readFileSync } = await import("node:fs");
  const { Gateway, createGatewayServer } = await import("./gateway.js");
  const gw = new Gateway(repo, { log: (m) => console.log(`gateway: ${m}`) });
  gw.startOutboxPublisher();
  const gport = Number(process.env.GATEWAY_PORT ?? 8443);
  createGatewayServer(gw, { cert: readFileSync(GATEWAY_CERT), key: readFileSync(GATEWAY_KEY), ca: readFileSync(GATEWAY_CA) })
    .listen(gport, () => console.log(`nurtuo runtime gateway (mTLS) listening on :${gport}`));
}
