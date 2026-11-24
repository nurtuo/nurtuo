// SPDX-License-Identifier: AGPL-3.0-only
// Connection and secret endpoints (auth-module endpoints keyed by policy action; they receive the
// workspace resolved from the URL, so a foreign workspace is already a 404 before they run).
import { ApiError } from "../domain.js";
import type { Endpoint, EndpointResult } from "../auth/endpoints.js";
import type { ConnectionRepository, Connection } from "./repository.js";
import { MAX_SECRET_BYTES, SECRET_NAME, type SecretStore } from "./secrets.js";
import { secretRefs, validateConnection, validateConnectionId } from "./validate.js";

export interface ConnectionDeps { connections: ConnectionRepository; secrets: SecretStore }

const notFound = (what: string) => new ApiError(404, "not_found", `${what} not found`);
const obj = (b: unknown): Record<string, unknown> => { if (!b || typeof b !== "object" || Array.isArray(b)) throw new ApiError(400, "bad_request", "body must be a JSON object"); return b as Record<string, unknown>; };

async function present(d: ConnectionDeps, ws: string, c: Connection) {
  const secrets: Record<string, "set" | "missing"> = {};
  for (const r of secretRefs(c.connector, c.settings)) secrets[r.field] = (await d.secrets.has(ws, r.name)) ? "set" : "missing";
  return { id: c.id, connector: c.connector, settings: c.settings, secrets, updatedAt: c.updatedAt };
}

export function connectionEndpoints(d: ConnectionDeps): Record<string, Endpoint> {
  return {
    async "connection.list"(c): Promise<EndpointResult> {
      const ws = c.workspace!.id;
      return { status: 200, body: { items: await Promise.all((await d.connections.list(ws)).map((x) => present(d, ws, x))) } };
    },
    async "connection.get"(c) {
      const ws = c.workspace!.id;
      const x = await d.connections.get(ws, c.params.id!);
      if (!x) throw notFound("connection");
      return { status: 200, body: await present(d, ws, x) };
    },
    async "connection.put"(c) {
      const ws = c.workspace!.id, id = c.params.id!;
      validateConnectionId(id);
      const b = obj(c.req.body);
      const settings = validateConnection(b.connector, b.settings);
      const before = await d.connections.get(ws, id);
      const r = await d.connections.put(ws, { id, connector: b.connector as string, settings });
      return {
        status: r.created ? 201 : 200, body: await present(d, ws, r.connection),
        // settings hold only secret:// references, so hashing them is safe
        audit: { resourceId: id, before: before && { connector: before.connector, settings: before.settings }, after: { connector: r.connection.connector, settings: r.connection.settings } },
      };
    },
    async "connection.delete"(c) {
      const ws = c.workspace!.id, id = c.params.id!;
      const before = await d.connections.get(ws, id);
      if (!before || !(await d.connections.delete(ws, id))) throw notFound("connection");
      return { status: 204, audit: { resourceId: id, before: { connector: before.connector, settings: before.settings } } };
    },

    async "secret.list"(c) { return { status: 200, body: { items: await d.secrets.list(c.workspace!.id) } }; },
    async "secret.put"(c) {
      const ws = c.workspace!.id, name = c.params.name!;
      if (!SECRET_NAME.test(name)) throw new ApiError(400, "bad_request", "secret name must match ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$");
      const b = obj(c.req.body);
      if (typeof b.value !== "string" || b.value === "") throw new ApiError(400, "bad_request", "value must be a non-empty string");
      if (Buffer.byteLength(b.value, "utf8") > MAX_SECRET_BYTES) throw new ApiError(400, "bad_request", "value is larger than 64 KiB");
      await d.secrets.put(ws, name, b.value);
      // The audit row names the secret and never carries a value or a hash of one.
      return { status: 204, audit: { resourceId: name, after: { name } } };
    },
    async "secret.delete"(c) {
      const ws = c.workspace!.id, name = c.params.name!;
      if (!(await d.secrets.delete(ws, name))) throw notFound("secret");
      return { status: 204, audit: { resourceId: name, before: { name } } };
    },
  };
}
