// SPDX-License-Identifier: AGPL-3.0-only
import { createHash, randomUUID } from "node:crypto";
import { isIP } from "node:net";
import { permissionsIn } from "../auth/authz.js";
import type { Endpoint, EndpointCtx } from "../auth/endpoints.js";
import { ApiError } from "../domain.js";
import { binding, checkSecretPlacement } from "./binding.js";
import { ConnectionCoordinator } from "./coordinator.js";
import { validHostKey } from "./host-key.js";
import { validatePrivateKey } from "./private-key.js";
import type { ConnectionRepository, Connection } from "./repository.js";
import { MAX_SECRET_BYTES, type SecretStore } from "./secrets.js";
import { secretFields, secretRefs, validateConnection, validateConnectionId } from "./validate.js";

export interface ConnectionDeps {
  connections: ConnectionRepository;
  secrets: SecretStore;
  coordinator?: ConnectionCoordinator;
  trustProxy?: boolean;
  proxyConfigured?: boolean;
}

const notFound = () => new ApiError(404, "not_found", "connection not found");
const obj = (b: unknown): Record<string, unknown> => {
  if (!b || typeof b !== "object" || Array.isArray(b)) throw new ApiError(400, "bad_request", "expected a JSON object");
  return b as Record<string, unknown>;
};

async function present(d: ConnectionDeps, ws: string, c: Connection) {
  const secrets: Record<string, "set" | "missing"> = {};
  for (const r of secretRefs(c.connector, c.settings)) secrets[r.field] = (await d.secrets.has(ws, r.name)) ? "set" : "missing";
  return { id: c.id, connector: c.connector, settings: c.settings, secrets, updatedAt: c.updatedAt };
}

function authorizeCredentials(c: EndpointCtx, credentials: Record<string, unknown>, trustProxy?: boolean, proxyConfigured?: boolean) {
  if (Object.keys(credentials).length === 0) return;
  const p = c.principal;
  if (p?.kind !== "user" || !p.viaCookie) throw new ApiError(403, "forbidden", "credential changes require a browser session");
  const permissions = permissionsIn(p, c.workspace!.id);
  for (const value of Object.values(credentials)) {
    if (!permissions.has(value === null ? "secret:delete" : "secret:write")) throw new ApiError(403, "forbidden", "credential permission is required");
  }
  const peer = c.req.ip ?? "";
  const loopback = peer === "::1" || (isIP(peer) && /^(?:::ffff:)?127\./i.test(peer));
  const trusted = trustProxy ? c.req.headers?.["x-forwarded-proto"] === "https" : !proxyConfigured && loopback;
  if (!trusted) throw new ApiError(403, "tls_required", "credential changes require HTTPS or a loopback connection");
}

function applyTrust(settings: Record<string, unknown>, input: Record<string, unknown>) {
  if (input.hostKeyPolicy === "known_hosts") throw new ApiError(400, "host_key_policy_not_supported", "paste one host key or enter its fingerprint");
  if (Object.hasOwn(input, "knownHosts")) {
    if (typeof input.knownHosts !== "string" || Buffer.byteLength(input.knownHosts) > 64 * 1024) {
      throw new ApiError(400, "known_hosts_ambiguous", "paste one host key or enter its fingerprint");
    }
    const host = String(settings.host).replace(/^\[|\]$/g, "").toLowerCase();
    if (/^\s*@/m.test(input.knownHosts)) {
      throw new ApiError(400, "known_hosts_ambiguous", "marked known_hosts entries require an explicit fingerprint");
    }
    const port = settings.port ?? 22;
    const target = port === 22 ? host : `[${host}]:${port}`;
    const matches = input.knownHosts.split(/\r?\n/)
      .map((line) => line.trim().split(/\s+/))
      .filter(([names]) => names?.toLowerCase().split(",").includes(target));
    const [entry] = matches;
    if (matches.length !== 1 || !entry?.[1]?.match(/^(ssh-|ecdsa-)/) || !entry[2]?.match(/^[A-Za-z0-9+/]+={0,2}$/)) {
      throw new ApiError(400, "known_hosts_ambiguous", "paste one host key or enter its fingerprint");
    }
    const key = Buffer.from(entry[2], "base64");
    if (!validHostKey(entry[1], key)) {
      throw new ApiError(400, "known_hosts_ambiguous", "paste one host key or enter its fingerprint");
    }
    settings.hostKeyPolicy = "fingerprint";
    settings.hostKeyFingerprint = `SHA256:${createHash("sha256").update(key).digest("base64").replace(/=+$/, "")}`;
    delete settings.knownHosts;
  }
}

export function connectionEndpoints(d: ConnectionDeps): Record<string, Endpoint> {
  const coordinator = d.coordinator ?? new ConnectionCoordinator();

  const reconcile = (ws: string, id: string) => coordinator.reconcile(d.connections, d.secrets, ws, id);

  return {
    async "connection.list"(c) {
      const ws = c.workspace!.id;
      return coordinator.withWorkspace(ws, async () => {
        for (const id of coordinator.pending.get(ws)?.keys() ?? []) await reconcile(ws, id);
        return { status: 200, body: { items: await Promise.all((await d.connections.list(ws)).map((x) => present(d, ws, x))) } };
      });
    },
    async "connection.get"(c) {
      const ws = c.workspace!.id;
      return coordinator.withWorkspace(ws, async () => {
        await reconcile(ws, c.params.id!);
        const x = await d.connections.get(ws, c.params.id!);
        if (!x) throw notFound();
        return { status: 200, body: await present(d, ws, x) };
      });
    },
    async "connection.put"(c) {
      const ws = c.workspace!.id, id = c.params.id!;
      validateConnectionId(id);
      const b = obj(c.req.body);
      const input = obj(b.settings);
      const connector = b.connector;
      if (connector !== "sftp") throw new ApiError(400, "not_allowed_in_test_run", "only SFTP connections are supported");
      const credentials = b.credentials === undefined ? {} : obj(b.credentials);
      const fields = secretFields(connector);
      checkSecretPlacement(connector, input, false);
      authorizeCredentials(c, credentials, d.trustProxy, d.proxyConfigured);
      for (const [field, value] of Object.entries(credentials)) {
        if (!fields.includes(field) || (value !== null && (typeof value !== "string" || !value || value.includes("\0") || Buffer.byteLength(value) > MAX_SECRET_BYTES))) {
          throw new ApiError(400, "invalid_credential", "credential must be a non-empty string up to 64 KiB or null");
        }
      }
      return coordinator.withWorkspace(ws, async () => {
        await reconcile(ws, id);
        if (!(await c.service.repo.getWorkspace(ws))) throw notFound();
        const before = await d.connections.get(ws, id);
        const settings = { ...before?.settings, ...input };
        const incarnation = before?.incarnation ?? randomUUID();
        const generation = randomUUID();
        const created = new Map<string, string>();
        const retired = [];
        for (const field of fields) {
          if (!Object.hasOwn(credentials, field)) continue;
          const previous = secretRefs(connector, settings).find((r) => r.field === field);
          if (previous) retired.push(previous.name);
          const value = credentials[field];
          if (typeof value === "string") {
            const name = `credential-${randomUUID()}`;
            settings[field] = `secret://${name}`;
            created.set(name, value);
          } else delete settings[field];
        }
        applyTrust(settings, input);
        if (input.hostKeyPolicy === "fingerprint") delete settings.knownHosts;
        if ((!settings.password && !settings.privateKey) || (settings.passphrase && !settings.privateKey)) {
          throw new ApiError(400, "invalid_credential_set", "enter a password or private key; a passphrase requires a private key");
        }
        const write = { id, connector, settings: validateConnection(connector, settings), incarnation, generation };
        if (write.settings.hostKeyPolicy !== "fingerprint") throw new ApiError(400, "host_key_policy_not_supported", "enter a host key fingerprint");
        const refs = secretRefs(connector, write.settings);
        const values = new Map<string, string>();
        for (const { field, name } of refs) {
          const digest = binding(write, field);
          if (!created.has(name) && (!before?.incarnation || binding(before, field) !== digest)) {
            throw new ApiError(409, "credential_rebind_required", "enter every retained credential when the destination, trust or authentication changes");
          }
          const value = created.get(name) ?? d.secrets.acquire(ws, name, digest);
          if (value !== undefined) values.set(field, value);
        }
        if (values.has("privateKey") && (Object.hasOwn(credentials, "privateKey") || Object.hasOwn(credentials, "passphrase"))) {
          await validatePrivateKey(values.get("privateKey")!, values.get("passphrase"));
        }
        const staged: string[] = [];
        try {
          for (const { field, name } of refs) {
            const value = created.get(name);
            if (value === undefined) continue;
            await d.secrets.put(ws, name, value, binding(write, field));
            staged.push(name);
          }
        } catch (error) {
          for (const name of staged) await d.secrets.delete(ws, name);
          throw error;
        }
        let result;
        try {
          result = await d.connections.put(ws, write, (isNew, append) =>
            c.auditMutation(isNew ? 201 : 200, { resourceId: id, after: refs.map(({ field }) => binding(write, field)) }, append),
          );
        } catch (error) {
          if (error instanceof ApiError && error.code === "audit_unavailable") {
            for (const name of staged) await d.secrets.delete(ws, name);
            throw error;
          }
          const saves = coordinator.pending.get(ws) ?? new Map();
          saves.set(id, { created: staged, retired, generation });
          coordinator.pending.set(ws, saves);
          const committed = await reconcile(ws, id);
          if (!committed) throw error;
          result = { connection: committed, created: !before };
        }
        for (const name of retired) await d.secrets.delete(ws, name);
        return {
          status: result.created ? 201 : 200, body: await present(d, ws, result.connection),
        };
      });
    },
    async "connection.delete"(c) {
      const ws = c.workspace!.id, id = c.params.id!;
      return coordinator.withWorkspace(ws, async () => {
        await reconcile(ws, id);
        const before = await d.connections.get(ws, id);
        if (!before || !(await d.connections.delete(ws, id))) throw notFound();
        for (const { name } of secretRefs(before.connector, before.settings)) await d.secrets.delete(ws, name);
        return { status: 204, audit: { resourceId: id } };
      });
    },
  };
}
