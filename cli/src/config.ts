// SPDX-License-Identifier: Apache-2.0
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

export interface StoredConfig {
  server?: string;
  token?: string;
}

type Env = Record<string, string | undefined>;

/** `NURTUO_CONFIG`, else %APPDATA%\nurtuo\config.json (Windows) or $XDG_CONFIG_HOME/nurtuo/config.json. */
export function configPath(env: Env, platform: string = process.platform): string {
  if (env["NURTUO_CONFIG"]) return env["NURTUO_CONFIG"];
  if (platform === "win32") return join(env["APPDATA"] ?? join(homedir(), "AppData", "Roaming"), "nurtuo", "config.json");
  return join(env["XDG_CONFIG_HOME"] ?? join(env["HOME"] ?? homedir(), ".config"), "nurtuo", "config.json");
}

export function readConfig(path: string): StoredConfig {
  if (!existsSync(path)) return {};
  try {
    const v = JSON.parse(readFileSync(path, "utf8")) as unknown;
    if (v && typeof v === "object" && !Array.isArray(v)) {
      const o = v as Record<string, unknown>;
      return {
        ...(typeof o["server"] === "string" ? { server: o["server"] } : {}),
        ...(typeof o["token"] === "string" ? { token: o["token"] } : {}),
      };
    }
  } catch {
    throw new Error(`cannot parse config file ${path}; fix or delete it`);
  }
  return {};
}

/** Writes the config with mode 0600 (best effort: POSIX modes are not enforced on Windows). */
export function writeConfig(path: string, cfg: StoredConfig): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify(cfg, null, 2) + "\n", { mode: 0o600 });
  try {
    chmodSync(path, 0o600); // writeFileSync's mode only applies when creating the file
  } catch {
    /* unsupported filesystem */
  }
}

export function removeConfig(path: string): boolean {
  if (!existsSync(path)) return false;
  rmSync(path);
  return true;
}

export interface Resolved {
  server?: string;
  token?: string;
  tokenSource?: "flag" | "env" | "config";
  serverSource?: "flag" | "env" | "config";
}

/** Precedence: flags > NURTUO_SERVER / NURTUO_TOKEN > config file. */
export function resolveConnection(env: Env, flags: { server?: string; token?: string }): Resolved {
  const file = readConfig(configPath(env));
  const out: Resolved = {};
  const pick = (k: "server" | "token", flag: string | undefined, envVar: string) => {
    const e = env[envVar];
    if (flag) { out[k] = flag; out[`${k}Source`] = "flag"; }
    else if (e) { out[k] = e; out[`${k}Source`] = "env"; }
    else if (file[k]) { out[k] = file[k]; out[`${k}Source`] = "config"; }
  };
  pick("server", flags.server, "NURTUO_SERVER");
  pick("token", flags.token, "NURTUO_TOKEN");
  return out;
}
