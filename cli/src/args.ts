// SPDX-License-Identifier: Apache-2.0
// Tiny argument parser: --flag, --flag value, --flag=value, -f value, -abc (booleans), `--` terminator.

export interface FlagSpec {
  type: "string" | "boolean";
  /** Single-letter alias, e.g. "o" for -o. */
  short?: string;
  /** Placeholder shown in help, e.g. "<file>". */
  value?: string;
  description: string;
}
export type FlagSpecs = Record<string, FlagSpec>;

export class UsageError extends Error {
  override readonly name = "UsageError";
}

export interface Parsed {
  positionals: string[];
  flags: Record<string, string | boolean>;
}

export function parseArgs(argv: readonly string[], specs: FlagSpecs): Parsed {
  const positionals: string[] = [];
  const flags: Record<string, string | boolean> = {};
  const byShort = new Map<string, string>();
  for (const [name, s] of Object.entries(specs)) if (s.short) byShort.set(s.short, name);

  const set = (name: string, spec: FlagSpec, raw: string | undefined, rest: { next(): string | undefined }, shown: string) => {
    if (spec.type === "boolean") {
      if (raw !== undefined) {
        if (raw !== "true" && raw !== "false") throw new UsageError(`flag ${shown} does not take a value`);
        flags[name] = raw === "true";
      } else flags[name] = true;
      return;
    }
    const v = raw ?? rest.next();
    if (v === undefined) throw new UsageError(`flag ${shown} requires a value`);
    flags[name] = v;
  };

  let i = 0;
  const rest = { next: () => (i + 1 < argv.length ? argv[++i] : undefined) };
  for (; i < argv.length; i++) {
    const a = argv[i]!;
    if (a === "--") {
      positionals.push(...argv.slice(i + 1));
      break;
    }
    if (a.startsWith("--")) {
      const eq = a.indexOf("=");
      const name = eq === -1 ? a.slice(2) : a.slice(2, eq);
      const spec = specs[name];
      if (!spec) throw new UsageError(`unknown flag --${name}`);
      set(name, spec, eq === -1 ? undefined : a.slice(eq + 1), rest, `--${name}`);
    } else if (a.length > 1 && a.startsWith("-") && !/^-\d/.test(a)) {
      const letters = a.slice(1);
      for (let j = 0; j < letters.length; j++) {
        const name = byShort.get(letters[j]!);
        if (!name) throw new UsageError(`unknown flag -${letters[j]}`);
        const spec = specs[name]!;
        if (spec.type === "boolean") flags[name] = true;
        else {
          // value is the remainder (-ofile) or the next argument
          const attached = letters.slice(j + 1);
          set(name, spec, attached || undefined, rest, `-${letters[j]}`);
          break;
        }
      }
    } else positionals.push(a);
  }
  return { positionals, flags };
}

export const str = (p: Parsed, name: string): string | undefined => {
  const v = p.flags[name];
  return typeof v === "string" ? v : undefined;
};
export const bool = (p: Parsed, name: string): boolean => p.flags[name] === true;
