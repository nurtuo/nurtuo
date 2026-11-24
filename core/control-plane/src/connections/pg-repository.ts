// SPDX-License-Identifier: AGPL-3.0-only
import type { Queryable } from "../pg-repository.js";
import type { Connection, ConnectionRepository } from "./repository.js";

const iso = (v: unknown) => (v instanceof Date ? v.toISOString() : (v as string));
const row = (r: any): Connection => ({ id: r.id, connector: r.connector, settings: r.settings, createdAt: iso(r.created_at), updatedAt: iso(r.updated_at) });

/** Postgres backend (migration 0300). Keyed by workspace id; workspace delete cascades. */
export class PgConnectionRepository implements ConnectionRepository {
  constructor(private db: Queryable) {}

  async list(ws: string) { return (await this.db.query("SELECT * FROM connections WHERE workspace_id=$1 ORDER BY id", [ws])).rows.map(row); }
  async get(ws: string, id: string) { const r = await this.db.query("SELECT * FROM connections WHERE workspace_id=$1 AND id=$2", [ws, id]); return r.rows[0] ? row(r.rows[0]) : undefined; }
  async put(ws: string, c: { id: string; connector: string; settings: Record<string, unknown> }) {
    const r = await this.db.query(
      `INSERT INTO connections(workspace_id, id, connector, settings) VALUES ($1,$2,$3,$4)
       ON CONFLICT (workspace_id, id) DO UPDATE SET connector=EXCLUDED.connector, settings=EXCLUDED.settings, updated_at=now()
       RETURNING *, (xmax = 0) AS inserted`,
      [ws, c.id, c.connector, JSON.stringify(c.settings)],
    );
    return { connection: row(r.rows[0]), created: Boolean(r.rows[0].inserted) };
  }
  async delete(ws: string, id: string) { const r = await this.db.query("DELETE FROM connections WHERE workspace_id=$1 AND id=$2", [ws, id]); return (r.rowCount ?? 0) > 0; }
  async purge(ws: string) { await this.db.query("DELETE FROM connections WHERE workspace_id=$1", [ws]); }
}
