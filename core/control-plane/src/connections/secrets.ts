// SPDX-License-Identifier: AGPL-3.0-only
// Workspace-scoped, write-only secret store (canvas-run spec D3). Only the in-memory backend exists in
// this slice: values live in the control-plane process, never on disk, and are lost on restart.
// reveal() is called only by the test runner; no API route returns a value.
import { ApiError } from "../domain.js";
import { envVar } from "./env-var.js";

/** Same rule as Go secrets.ValidName / the `secret://<name>` grammar. */
export const SECRET_NAME = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/;
export const SECRET_REF = /^secret:\/\/([A-Za-z0-9][A-Za-z0-9_.-]{0,127})$/;
export const MAX_SECRET_BYTES = 64 * 1024;

export interface SecretInfo { name: string; updatedAt: string }
export interface SecretStore {
  acquire(workspaceId: string, name: string, binding: string): string | undefined;
  put(workspaceId: string, name: string, value: string, binding?: string): Promise<void>;
  has(workspaceId: string, name: string): Promise<boolean>;
  list(workspaceId: string): Promise<SecretInfo[]>;
  delete(workspaceId: string, name: string): Promise<boolean>;
  /** The only way to read a value; used by the test runner and nothing else. */
  reveal(workspaceId: string, name: string): Promise<string | undefined>;
  /** Removes every secret of a workspace (workspace delete). */
  purge(workspaceId: string): Promise<void>;
}

export class MemorySecretStore implements SecretStore {
  private byWs = new Map<string, Map<string, { value: Buffer; updatedAt: string; binding?: string }>>();

  acquire(ws: string, name: string, binding: string) {
    const entry = this.byWs.get(ws)?.get(name);
    if (entry && entry.binding !== binding) throw new ApiError(409, "credential_unbound", "credential does not match the connection");
    return entry?.value.toString("utf8");
  }

  async put(ws: string, name: string, value: string, binding?: string) {
    if (!SECRET_NAME.test(name)) throw new ApiError(400, "bad_request", "invalid secret name");
    const m = this.byWs.get(ws) ?? new Map();
    // Two names that map to one environment variable cannot both reach the child (D12).
    for (const other of m.keys()) if (other !== name && envVar(other) === envVar(name)) throw new ApiError(409, "secret_name_collision", `secret name collides with "${other}" (same environment variable)`);
    if (m.has(name) && (binding !== undefined || m.get(name)?.binding !== undefined)) throw new ApiError(409, "credential_unbound", "credential versions are immutable");
    m.set(name, { binding, value: Buffer.from(value, "utf8"), updatedAt: new Date().toISOString() });
    this.byWs.set(ws, m);
  }
  async has(ws: string, name: string) { return this.byWs.get(ws)?.has(name) ?? false; }
  async list(ws: string) {
    return [...(this.byWs.get(ws) ?? [])].map(([name, v]) => ({ name, updatedAt: v.updatedAt })).sort((a, b) => a.name.localeCompare(b.name));
  }
  async delete(ws: string, name: string) { return this.byWs.get(ws)?.delete(name) ?? false; }
  async reveal(ws: string, name: string) { return this.byWs.get(ws)?.get(name)?.value.toString("utf8"); }
  async purge(ws: string) { this.byWs.delete(ws); }
}
