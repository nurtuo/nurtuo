// SPDX-License-Identifier: AGPL-3.0-only
import type { AppendAudit } from "../auth/endpoints.js";

// Workspace connections (nurtuo.connection/v1 without the secret values). Every method takes the
// workspace id first: a connection of another workspace is indistinguishable from a missing one.
export interface Connection {
  id: string;
  connector: string;
  incarnation?: string;
  generation?: string;
  /** Non-secret settings; secret fields hold `secret://<name>` references only. */
  settings: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
}

export type ConnectionWrite = Pick<Connection, "id" | "connector" | "settings" | "incarnation" | "generation">;

export type AuditConnectionWrite = (created: boolean, append?: AppendAudit) => Promise<void>;

export interface ConnectionRepository {
  list(workspaceId: string): Promise<Connection[]>;
  get(workspaceId: string, id: string): Promise<Connection | undefined>;
  put(workspaceId: string, c: ConnectionWrite, audit?: AuditConnectionWrite): Promise<{ connection: Connection; created: boolean }>;
  delete(workspaceId: string, id: string): Promise<boolean>;
  /** Removes every connection of a workspace (the Postgres backend cascades on its own). */
  purge(workspaceId: string): Promise<void>;
}

export class MemoryConnectionRepository implements ConnectionRepository {
  private byWs = new Map<string, Map<string, Connection>>();

  async list(ws: string) { return [...(this.byWs.get(ws)?.values() ?? [])].map((c) => structuredClone(c)).sort((a, b) => a.id.localeCompare(b.id)); }
  async get(ws: string, id: string) { const c = this.byWs.get(ws)?.get(id); return c && structuredClone(c); }
  async put(ws: string, c: ConnectionWrite, audit?: AuditConnectionWrite) {
    const m = this.byWs.get(ws) ?? new Map<string, Connection>();
    const prev = m.get(c.id);
    const now = new Date().toISOString();
    const connection: Connection = { ...c, settings: structuredClone(c.settings), createdAt: prev?.createdAt ?? now, updatedAt: now };
    if (audit) await audit(!prev);
    m.set(c.id, connection);
    this.byWs.set(ws, m);
    return { connection: structuredClone(connection), created: !prev };
  }
  async delete(ws: string, id: string) { return this.byWs.get(ws)?.delete(id) ?? false; }
  async purge(ws: string) { this.byWs.delete(ws); }
}
