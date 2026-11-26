// SPDX-License-Identifier: AGPL-3.0-only
import { appendPgAudit } from "../auth/pg-repository.js";
import type { Db, Queryable } from "../pg-repository.js";
import type { AuditConnectionWrite, Connection, ConnectionRepository, ConnectionWrite } from "./repository.js";

const iso = (v: unknown) => (v instanceof Date ? v.toISOString() : (v as string));
const row = (r: any): Connection => ({ id: r.id, connector: r.connector, settings: r.settings, incarnation: r.incarnation, generation: r.generation, createdAt: iso(r.created_at), updatedAt: iso(r.updated_at) });

/** Postgres backend (migration 0300). Keyed by workspace id; workspace delete cascades. */
export class PgConnectionRepository implements ConnectionRepository {
  constructor(private db: Db) {}

  async list(ws: string) { return (await this.db.query("SELECT * FROM connections WHERE workspace_id=$1 ORDER BY id", [ws])).rows.map(row); }
  async get(ws: string, id: string) { const r = await this.db.query("SELECT * FROM connections WHERE workspace_id=$1 AND id=$2", [ws, id]); return r.rows[0] ? row(r.rows[0]) : undefined; }
  async put(ws: string, c: ConnectionWrite, audit?: AuditConnectionWrite) {
    if (!audit) return this.write(this.db, ws, c);
    const tx = await this.db.connect();
    try {
      await tx.query("BEGIN");
      const result = await this.write(tx, ws, c);
      await audit(result.created, (entry) => appendPgAudit(tx, entry));
      await tx.query("COMMIT");
      return result;
    } catch (error) {
      await tx.query("ROLLBACK").catch(() => {
        // Preserve the write or audit error if the transaction connection failed.
      });
      throw error;
    } finally {
      tx.release();
    }
  }
  private async write(db: Queryable, ws: string, c: ConnectionWrite) {
    const r = await db.query(
      `INSERT INTO connections(workspace_id, id, connector, settings, incarnation, generation) VALUES ($1,$2,$3,$4,$5,$6)
       ON CONFLICT (workspace_id, id) DO UPDATE SET connector=EXCLUDED.connector, settings=EXCLUDED.settings, incarnation=EXCLUDED.incarnation, generation=EXCLUDED.generation, updated_at=now()
       RETURNING *, (xmax = 0) AS inserted`,
      [ws, c.id, c.connector, JSON.stringify(c.settings), c.incarnation, c.generation],
    );
    return { connection: row(r.rows[0]), created: Boolean(r.rows[0].inserted) };
  }
  async delete(ws: string, id: string) { const r = await this.db.query("DELETE FROM connections WHERE workspace_id=$1 AND id=$2", [ws, id]); return (r.rowCount ?? 0) > 0; }
  async purge(ws: string) { await this.db.query("DELETE FROM connections WHERE workspace_id=$1", [ws]); }
}
