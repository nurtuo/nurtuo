// SPDX-License-Identifier: AGPL-3.0-only
// TypeScript port of Go secrets.EnvVar (core/runtime/internal/secrets/env.go), prefix included.
// Pinned by core/runtime/internal/secrets/testdata/envvar.json, which the Go test also reads.
export const ENV_PREFIX = "NURTUO_SECRET_";

/** The environment variable that carries secret `name` to a `nurtuo flow run` child. */
export function envVar(name: string): string {
  let out = ENV_PREFIX;
  for (const c of name.toUpperCase()) out += /[A-Z0-9]/.test(c) ? c : "_";
  return out;
}
