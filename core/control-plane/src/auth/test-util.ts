// SPDX-License-Identifier: AGPL-3.0-only
// Test harness (not imported by production code): a fully wired handler with in-memory repos and a movable clock.
import { randomBytes } from "node:crypto";
import { createHandler, type ApiResponse } from "../api.js";
import { MemoryConnectionRepository } from "../connections/repository.js";
import { ConnectionCoordinator } from "../connections/coordinator.js";
import { connectionEndpoints } from "../connections/routes.js";
import { MemorySecretStore } from "../connections/secrets.js";
import { MemoryRepository } from "../memory-repository.js";
import { generateApiToken } from "./crypto.js";
import type { InstanceConfig } from "./instance.js";
import { createAuthHook, type AuthHookOptions } from "./middleware.js";
import { MemoryAuthRepository } from "./memory-repository.js";
import { ROLE_PERMISSIONS, type Permission, type Role } from "./permissions.js";
import { AuthService, SESSION_COOKIE, type AuthServiceOptions } from "./service.js";

export const PASSWORD = "correct horse battery staple";
export interface CallOpts { body?: unknown; headers?: Record<string, string>; query?: Record<string, string>; token?: string; cookie?: string; csrf?: string; ws?: string; ip?: string }
export const j = (r: ApiResponse) => r.body as any;
/** Instance config with these emails as instance admins (mode "admins"); pass to setup({ service: { instance } }). */
export const adminsConfig = (...emails: string[]): InstanceConfig => ({ admins: new Set(emails.map((e) => e.trim().toLowerCase())), workspaceCreate: "admins" });

/** An SFTP connection body that passes validation. */
export const sftpConn = (over: Record<string, unknown> = {}) => ({
  connector: "sftp",
  credentials: { password: "pw-value-123" },
  settings: { host: "sftp.example.com", port: 22, user: "u", hostKeyPolicy: "fingerprint", hostKeyFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ...over },
});
export function setup(over: { service?: Partial<AuthServiceOptions>; hook?: Partial<AuthHookOptions> } = {}) {
  const repo = new MemoryRepository();
  const authRepo = new MemoryAuthRepository();
  const clock = { t: Date.parse("2026-01-01T00:00:00Z") };
  const service = new AuthService(authRepo, { secret: randomBytes(32), scrypt: { N: 1024, r: 8, p: 1 }, now: () => new Date(clock.t), loginsPerMinutePerIp: 10_000, ...over.service });
  const connections = new MemoryConnectionRepository();
  const secrets = new MemorySecretStore();
  const coordinator = new ConnectionCoordinator();
  const hook = createAuthHook({
    service,
    extraEndpoints: { ...connectionEndpoints({ connections, secrets, coordinator, trustProxy: over.hook?.trustProxy }) },
    serializeWorkspaceDelete: (id, remove) => coordinator.withWorkspace(id, async () => {
        const result = await remove();
        if (result.status < 300) {
          await connections.purge(id);
          await secrets.purge(id);
          coordinator.pending.delete(id);
        }
        return result;
      }),
    ...over.hook,
  });
  const handle = createHandler(repo, { auth: hook });

  const call = (method: string, path: string, o: CallOpts = {}) => {
    const headers: Record<string, string> = { ...o.headers };
    if (o.token) headers.authorization = `Bearer ${o.token}`;
    if (o.cookie) headers.cookie = `${SESSION_COOKIE}=${o.cookie}`;
    if (o.csrf) headers["x-nurtuo-csrf"] = o.csrf;
    if (o.ws) headers["x-nurtuo-workspace"] = o.ws;
    return handle({ method, path, body: o.body, query: o.query, headers, ip: o.ip ?? "10.0.0.1" });
  };

  /** Creates a user (and optionally a workspace they own). */
  async function owner(email: string, slug: string) {
    const user = await service.createPasswordUser(email, PASSWORD);
    const ws = await authRepo.createWorkspace({ slug, name: slug, ownerId: user.id });
    return { user, ws };
  }
  /** Adds a member with `role` and returns an API token carrying all of that role's permissions. */
  async function member(wsId: string, email: string, role: Role, scopes?: Permission[], ttlMs = 86400_000) {
    const user = (await authRepo.getUserByEmail(email)) ?? (await service.createPasswordUser(email, PASSWORD));
    await authRepo.setMember(wsId, user.id, role);
    const token = await mint(wsId, user.id, scopes ?? [...ROLE_PERMISSIONS[role]], ttlMs);
    return { user, token };
  }
  async function mint(wsId: string, userId: string, scopes: Permission[], ttlMs = 86400_000) {
    const g = generateApiToken();
    const rec = await authRepo.createToken({ workspaceId: wsId, createdBy: userId, name: "t", prefix: g.prefix, hash: g.hash, scopes, expiresAt: new Date(clock.t + ttlMs).toISOString() });
    return { raw: g.token, id: rec.id };
  }
  async function login(email: string, password = PASSWORD) {
    const r = await call("POST", "/v1/auth/login", { body: { email, password } });
    const cookie = /__Host-nurtuo_session=([^;]+)/.exec(String((r.headers?.["set-cookie"] as string[] | undefined)?.[0] ?? ""))?.[1];
    return { r, cookie: cookie!, csrf: j(r)?.csrfToken as string };
  }
  return { repo, authRepo, service, handle, call, connections, secrets, clock, owner, member, mint, login };
}
