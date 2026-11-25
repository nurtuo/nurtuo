// SPDX-License-Identifier: AGPL-3.0-only
// Egress check for server-side test runs (canvas-run spec D11): refuse hosts that resolve to loopback,
// private, link-local, carrier-grade NAT or metadata ranges unless the operator allows them.
import { Resolver } from "node:dns/promises";
import { isIP } from "node:net";
import { logicalHost } from "../connections/binding.js";
import { ApiError } from "../domain.js";

export type Lookup = (host: string, signal?: AbortSignal) => Promise<string[]>;

export const defaultLookup: Lookup = async (host, signal) => {
  signal?.throwIfAborted();
  const resolver = new Resolver({ timeout: 2000, tries: 2 });
  const cancel = () => resolver.cancel();
  signal?.addEventListener("abort", cancel, { once: true });
  try {
    const resolveFamily = async (answers: Promise<string[]>) => {
      try { return await answers; }
      catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENODATA") return [];
        throw error;
      }
    };
    const families = await Promise.all([resolveFamily(resolver.resolve4(`${host}.`)), resolveFamily(resolver.resolve6(`${host}.`))]);
    return families.flat();
  } finally {
    signal?.removeEventListener("abort", cancel);
    resolver.cancel();
  }
};

async function lookupUntilAbort(lookup: Lookup, host: string, signal?: AbortSignal): Promise<string[]> {
  signal?.throwIfAborted();
  if (!signal) return lookup(host);
  let cancel: () => void = () => {};
  try {
    const aborted = new Promise<never>((_, reject) => {
      cancel = () => reject(signal.reason);
      signal.addEventListener("abort", cancel, { once: true });
    });
    return await Promise.race([aborted, lookup(host, signal)]);
  } finally {
    signal.removeEventListener("abort", cancel);
  }
}

/** Expands an IPv6 literal into eight 16-bit groups (an embedded dotted IPv4 tail is supported). */
function groups6(ip: string): number[] | undefined {
  let s = ip.split("%")[0]!.toLowerCase();
  const v4 = /(\d+\.\d+\.\d+\.\d+)$/.exec(s);
  if (v4) {
    const o = v4[1]!.split(".").map(Number);
    if (o.some((n) => n > 255)) return undefined;
    s = s.slice(0, -v4[1]!.length) + ((o[0]! << 8) | o[1]!).toString(16) + ":" + ((o[2]! << 8) | o[3]!).toString(16);
  }
  const halves = s.split("::");
  if (halves.length > 2) return undefined;
  const head = halves[0] ? halves[0].split(":") : [];
  const tail = halves.length === 2 && halves[1] ? halves[1].split(":") : [];
  const fill = halves.length === 2 ? 8 - head.length - tail.length : 0;
  if (fill < 0 || (halves.length === 1 && head.length !== 8)) return undefined;
  const all = [...head, ...Array<string>(fill).fill("0"), ...tail].map((h) => parseInt(h, 16));
  return all.length === 8 && all.every((n) => Number.isInteger(n) && n >= 0 && n <= 0xffff) ? all : undefined;
}

function blockedV4(o: number[]): boolean {
  const [a, b, c] = [o[0]!, o[1]!, o[2]!];
  return a === 0 || a === 127 || a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) ||
    (a === 100 && b >= 64 && b <= 127) || (a === 169 && b === 254) ||
    (a === 192 && b === 0 && c === 0) || (a === 198 && (b === 18 || b === 19)) || a >= 224;
}

/** True for addresses a test run must not dial by default. Unparseable input is blocked (fail closed). */
export function isBlockedAddress(ip: string): boolean {
  const kind = isIP(ip.split("%")[0]!);
  if (kind === 4) return blockedV4(ip.split(".").map(Number));
  if (kind === 6) {
    const g = groups6(ip);
    if (!g) return true;
    if (g.every((n, i) => (i < 7 ? n === 0 : n <= 1))) return true; // :: and ::1
    if (g.slice(0, 5).every((n) => n === 0) && g[5] === 0xffff) return blockedV4([g[6]! >> 8, g[6]! & 255, g[7]! >> 8, g[7]! & 255]); // ::ffff:a.b.c.d
    // Block transition and translation prefixes whose routing can reach another address family.
    if (g.slice(0, 6).every((n) => n === 0)) return true; // IPv4-compatible
    if (g[0] === 0x64 && g[1] === 0xff9b && g.slice(2, 6).every((n) => n === 0)) return true; // NAT64
    if (g[0] === 0x2002) return true; // 6to4
    if ((g[0]! & 0xfe00) === 0xfc00) return true; // fc00::/7 (incl. fd00:ec2::254)
    if ((g[0]! & 0xffc0) === 0xfe80) return true; // fe80::/10
    return false;
  }
  return true;
}

export interface EgressOptions { allowPrivate: boolean; lookup?: Lookup; signal?: AbortSignal }

/** Throws 400 host_not_allowed when `host` resolves to a refused address (or does not resolve). */
export async function checkEgress(host: string, o: EgressOptions): Promise<string> {
  o.signal?.throwIfAborted();
  const bare = logicalHost(host).replace(/^\[|\]$/g, "");
  let addrs: string[];
  try { addrs = isIP(bare) ? [bare] : await lookupUntilAbort(o.lookup ?? defaultLookup, bare, o.signal); }
  catch { o.signal?.throwIfAborted(); throw new ApiError(400, "host_not_allowed", `host "${host}" could not be resolved`); }
  if (addrs.length === 0 || addrs.some((address) => !isIP(address))) throw new ApiError(400, "host_not_allowed", `host "${host}" could not be resolved`);
  if (!o.allowPrivate && addrs.some(isBlockedAddress)) throw new ApiError(400, "host_not_allowed", `host "${host}" resolves to a loopback, private or link-local address; ask the operator to set NURTUO_TEST_RUN_ALLOW_PRIVATE=1 if this is intended`);
  return addrs[0]!;
}
