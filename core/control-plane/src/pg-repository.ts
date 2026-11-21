// SPDX-License-Identifier: AGPL-3.0-only
import { randomUUID } from "node:crypto";
import { readFile, readdir } from "node:fs/promises";
import { ApiError, type Component, type DesiredState, type Deployment, type OutboxEntry, type Revision, type Runtime } from "./domain.js";
import type { Repository } from "./repository.js";

/** Minimal shape satisfied by pg.Pool / pg.PoolClient. */
export interface Queryable { query(text: string, params?: unknown[]): Promise<{ rows: any[]; rowCount: number | null }> }
export interface Db extends Queryable { connect(): Promise<Queryable & { release(): void }> }

const iso = (v: unknown) => (v instanceof Date ? v.toISOString() : (v as string));
const comp = (r: any): Component => ({ id: r.id, name: r.name, kind: r.kind, description: r.description, createdAt: iso(r.created_at) });
const rev = (r: any): Revision => ({ id: r.id, componentId: r.component_id, number: r.number, digest: r.digest, content: r.content, createdAt: iso(r.created_at) });
const rt = (r: any): Runtime => ({ id: r.id, name: r.name, environment: r.environment, status: r.status, version: r.version, labels: r.labels, lastSeenAt: r.last_seen_at ? iso(r.last_seen_at) : null });
const ds = (r: any): DesiredState => ({ runtimeId: r.runtime_id, environment: r.environment, generation: Number(r.generation), items: r.items, updatedAt: iso(r.updated_at) });
const dep = (r: any): Deployment => ({ id: r.id, componentId: r.component_id, revisionId: r.revision_id, runtimeId: r.runtime_id, environment: r.environment, generation: Number(r.generation), status: r.status, message: r.message, createdAt: iso(r.created_at), updatedAt: iso(r.updated_at) });

/** Applies migrations/*.sql in filename order, recording them in schema_migrations. */
export async function migrate(db: Db, dir: string): Promise<string[]> {
  await db.query("CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())");
  const applied = new Set((await db.query("SELECT name FROM schema_migrations")).rows.map((r) => r.name));
  const done: string[] = [];
  for (const f of (await readdir(dir)).filter((f) => f.endsWith(".sql")).sort()) {
    if (applied.has(f)) continue;
    const c = await db.connect();
    try {
      await c.query("BEGIN");
      await c.query(await readFile(`${dir}/${f}`, "utf8"));
      await c.query("INSERT INTO schema_migrations(name) VALUES ($1)", [f]);
      await c.query("COMMIT");
      done.push(f);
    } catch (e) { await c.query("ROLLBACK"); throw e; } finally { c.release(); }
  }
  return done;
}

export class PgRepository implements Repository {
  constructor(private db: Db) {}

  private async tx<T>(fn: (q: Queryable) => Promise<T>): Promise<T> {
    const c = await this.db.connect();
    try { await c.query("BEGIN"); const v = await fn(c); await c.query("COMMIT"); return v; }
    catch (e) { await c.query("ROLLBACK"); throw e; } finally { c.release(); }
  }

  async createComponent(c: { name: string; kind: Component["kind"]; description: string }) {
    try {
      const r = await this.db.query("INSERT INTO components(id,name,kind,description) VALUES ($1,$2,$3,$4) RETURNING *", [randomUUID(), c.name, c.kind, c.description]);
      return comp(r.rows[0]);
    } catch (e: any) {
      if (e?.code === "23505") throw new ApiError(409, "conflict", `component ${c.name} exists`);
      throw e;
    }
  }
  async getComponent(id: string) { const r = await this.db.query("SELECT * FROM components WHERE id=$1", [id]); return r.rows[0] ? comp(r.rows[0]) : undefined; }
  async listComponents() { return (await this.db.query("SELECT * FROM components ORDER BY name")).rows.map(comp); }
  async updateComponent(id: string, p: { description?: string }) {
    const r = await this.db.query("UPDATE components SET description=COALESCE($2,description) WHERE id=$1 RETURNING *", [id, p.description ?? null]);
    return r.rows[0] ? comp(r.rows[0]) : undefined;
  }
  async deleteComponent(id: string) {
    if ((await this.db.query("SELECT 1 FROM revisions WHERE component_id=$1 LIMIT 1", [id])).rows.length) return "has_revisions" as const;
    const r = await this.db.query("DELETE FROM components WHERE id=$1", [id]);
    return r.rowCount ? ("ok" as const) : ("not_found" as const);
  }

  async createRevision(componentId: string, content: unknown, digest: string) {
    return this.tx(async (q) => {
      const c = await q.query("SELECT id FROM components WHERE id=$1 FOR UPDATE", [componentId]);
      if (!c.rows[0]) return undefined;
      const r = await q.query(
        "INSERT INTO revisions(id,component_id,number,digest,content) VALUES ($1,$2,(SELECT COALESCE(MAX(number),0)+1 FROM revisions WHERE component_id=$2),$3,$4::jsonb) RETURNING *",
        [randomUUID(), componentId, digest, JSON.stringify(content)]);
      return rev(r.rows[0]);
    });
  }
  async getRevision(componentId: string, number: number) { const r = await this.db.query("SELECT * FROM revisions WHERE component_id=$1 AND number=$2", [componentId, number]); return r.rows[0] ? rev(r.rows[0]) : undefined; }
  async getRevisionById(id: string) { const r = await this.db.query("SELECT * FROM revisions WHERE id=$1", [id]); return r.rows[0] ? rev(r.rows[0]) : undefined; }
  async listRevisions(componentId: string) { return (await this.db.query("SELECT * FROM revisions WHERE component_id=$1 ORDER BY number", [componentId])).rows.map(rev); }

  async upsertRuntime(r: { id: string; name: string; environment: string; version?: string; labels?: Record<string, string> }) {
    const x = await this.db.query(
      `INSERT INTO runtimes(id,name,environment,status,version,labels,last_seen_at) VALUES ($1,$2,$3,'online',$4,$5::jsonb,now())
       ON CONFLICT (id) DO UPDATE SET name=$2, environment=$3, status='online', version=$4, labels=$5::jsonb, last_seen_at=now() RETURNING *`,
      [r.id, r.name, r.environment, r.version ?? "", JSON.stringify(r.labels ?? {})]);
    return rt(x.rows[0]);
  }
  async getRuntime(id: string) { const r = await this.db.query("SELECT * FROM runtimes WHERE id=$1", [id]); return r.rows[0] ? rt(r.rows[0]) : undefined; }
  async listRuntimes(environment?: string) { return (await this.db.query("SELECT * FROM runtimes WHERE ($1::text IS NULL OR environment=$1) ORDER BY name", [environment ?? null])).rows.map(rt); }

  async getDesiredState(runtimeId: string) { const r = await this.db.query("SELECT * FROM desired_state WHERE runtime_id=$1", [runtimeId]); return r.rows[0] ? ds(r.rows[0]) : undefined; }
  async deploy(input: { componentId: string; revisionId: string; runtimeId: string }) {
    return this.tx(async (q) => {
      const r = (await q.query("SELECT * FROM runtimes WHERE id=$1", [input.runtimeId])).rows[0];
      if (!r) throw new ApiError(404, "not_found", "runtime not found");
      const v = await q.query("SELECT 1 FROM revisions WHERE id=$1 AND component_id=$2", [input.revisionId, input.componentId]);
      if (!v.rows[0]) throw new ApiError(404, "not_found", "revision not found for component");
      const d = (await q.query(
        `INSERT INTO desired_state(runtime_id,environment,generation,items) VALUES ($1,$2,1,jsonb_build_object($3::text,$4::text))
         ON CONFLICT (runtime_id) DO UPDATE SET generation=desired_state.generation+1,
           items=desired_state.items || jsonb_build_object($3::text,$4::text), updated_at=now() RETURNING *`,
        [r.id, r.environment, input.componentId, input.revisionId])).rows[0];
      const dp = (await q.query(
        "INSERT INTO deployments(id,component_id,revision_id,runtime_id,environment,generation) VALUES ($1,$2,$3,$4,$5,$6) RETURNING *",
        [randomUUID(), input.componentId, input.revisionId, r.id, r.environment, d.generation])).rows[0];
      await q.query("INSERT INTO outbox(topic,payload) VALUES ('desired_state.changed',$1::jsonb)", [JSON.stringify({ runtimeId: r.id, generation: Number(d.generation) })]);
      return { deployment: dep(dp), desiredState: ds(d) };
    });
  }
  async undeploy(input: { componentId: string; runtimeId: string }) {
    return this.tx(async (q) => {
      const d = (await q.query(
        `UPDATE desired_state SET generation=generation+1, items=items - $2::text, updated_at=now()
         WHERE runtime_id=$1 AND items ? $2::text RETURNING *`, [input.runtimeId, input.componentId])).rows[0];
      if (!d) return undefined;
      await q.query("INSERT INTO outbox(topic,payload) VALUES ('desired_state.changed',$1::jsonb)", [JSON.stringify({ runtimeId: input.runtimeId, generation: Number(d.generation) })]);
      return ds(d);
    });
  }
  async getDeployment(id: string) { const r = await this.db.query("SELECT * FROM deployments WHERE id=$1", [id]); return r.rows[0] ? dep(r.rows[0]) : undefined; }
  async listDeployments(f: { runtimeId?: string; componentId?: string }) {
    return (await this.db.query("SELECT * FROM deployments WHERE ($1::text IS NULL OR runtime_id=$1) AND ($2::text IS NULL OR component_id=$2) ORDER BY created_at DESC", [f.runtimeId ?? null, f.componentId ?? null])).rows.map(dep);
  }
  async updateDeploymentStatus(id: string, status: Deployment["status"], message: string) {
    const r = await this.db.query("UPDATE deployments SET status=$2,message=$3,updated_at=now() WHERE id=$1 RETURNING *", [id, status, message]);
    return r.rows[0] ? dep(r.rows[0]) : undefined;
  }
  async markOutboxPublished(id: number) {
    const r = await this.db.query("UPDATE outbox SET published_at=now() WHERE id=$1 AND published_at IS NULL", [id]);
    return (r.rowCount ?? 0) > 0;
  }
  async touchRuntime(id: string, version?: string) {
    const r = await this.db.query("UPDATE runtimes SET status='online', last_seen_at=now(), version=COALESCE(NULLIF($2,''),version) WHERE id=$1 RETURNING *", [id, version ?? ""]);
    return r.rows[0] ? rt(r.rows[0]) : undefined;
  }
  async markRuntimeOffline(id: string) { await this.db.query("UPDATE runtimes SET status='offline' WHERE id=$1", [id]); }
  async listOutbox(opts: { unpublishedOnly: boolean }): Promise<OutboxEntry[]> {
    const r = await this.db.query(`SELECT * FROM outbox ${opts.unpublishedOnly ? "WHERE published_at IS NULL" : ""} ORDER BY id`);
    return r.rows.map((x) => ({ id: Number(x.id), topic: x.topic, payload: x.payload, createdAt: iso(x.created_at), publishedAt: x.published_at ? iso(x.published_at) : null }));
  }
}
