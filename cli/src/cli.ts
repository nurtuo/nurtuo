// SPDX-License-Identifier: Apache-2.0
// nurtuoctl command registry and dispatcher. `run()` is pure with respect to the process: all I/O
// goes through the injected `Io`, so commands can be tested in-process or by spawning the binary.
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { ApiError, NetworkError, NurtuoClient, type Component, type ComponentKind, type Deployment, type Revision } from "@nurtuo/client";
import { hasErrors, validateFlow, type Diagnostic } from "@nurtuo/flow-validate";
import { bool, parseArgs, str, UsageError, type FlagSpecs, type Parsed } from "./args.js";
import { generateCompletion, SHELLS, type Shell, type TreeNode } from "./completion.js";
import { configPath, removeConfig, resolveConnection, writeConfig } from "./config.js";
import { canonicalJson, diffFlows, formatFlow, renderDiff, scaffold, slugify, TEMPLATES, type TemplateName } from "./flowtools.js";

/** Exit codes. */
export const EXIT = { OK: 0, FAILURE: 1, USAGE: 2, NETWORK: 3, AUTH: 4 } as const;

export interface Io {
  stdout(s: string): void;
  stderr(s: string): void;
  env: Record<string, string | undefined>;
  readStdin(): Promise<string>;
}

/** A failure that should end the command with a message and a specific exit code. */
export class CliError extends Error {
  override readonly name = "CliError";
  constructor(message: string, readonly exit: number = EXIT.FAILURE, readonly code = "error") {
    super(message);
  }
}

interface Ctx {
  io: Io;
  p: Parsed;
  json: boolean;
  client(): NurtuoClient;
  out(data: unknown, text: string): void;
  info(s: string): void;
}

interface Command {
  path: string[];
  summary: string;
  /** Argument synopsis after the command name, e.g. "<file> [-o <file>]". */
  usage: string;
  flags?: FlagSpecs;
  examples?: string[];
  details?: string;
  run?(ctx: Ctx): Promise<number | void>;
}

const GLOBAL_FLAGS: FlagSpecs = {
  json: { type: "boolean", description: "machine-readable JSON on stdout (errors as JSON on stderr)" },
  server: { type: "string", value: "<url>", description: "control-plane URL (overrides NURTUO_SERVER and the config file)" },
  token: { type: "string", value: "<token>", description: "API token (overrides NURTUO_TOKEN and the config file)" },
  help: { type: "boolean", short: "h", description: "show help for this command" },
};

// ------------------------------------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------------------------------------

function table(rows: string[][], header: string[]): string {
  const all = [header, ...rows];
  const w = header.map((_, i) => Math.max(...all.map((r) => (r[i] ?? "").length)));
  return all.map((r) => r.map((c, i) => (i === r.length - 1 ? c : c.padEnd(w[i]!))).join("  ").trimEnd()).join("\n") + "\n";
}

async function readInput(ctx: Ctx, file: string): Promise<string> {
  if (file === "-") return ctx.io.readStdin();
  try {
    return readFileSync(file, "utf8");
  } catch (e) {
    throw new CliError(`cannot read ${file}: ${(e as NodeJS.ErrnoException).code === "ENOENT" ? "no such file" : (e as Error).message}`, EXIT.FAILURE, "read_failed");
  }
}

async function readJson(ctx: Ctx, file: string): Promise<unknown> {
  const text = await readInput(ctx, file);
  try {
    return JSON.parse(text);
  } catch (e) {
    throw new CliError(`${file}: invalid JSON: ${(e as Error).message}`, EXIT.FAILURE, "invalid_json");
  }
}

function needArgs(ctx: Ctx, cmd: Command, n: number, max = n): string[] {
  const a = ctx.p.positionals;
  if (a.length < n) throw new UsageError(`missing argument; usage: nurtuoctl ${cmd.path.join(" ")} ${cmd.usage}`);
  if (a.length > max) throw new UsageError(`unexpected argument "${a[max]}"; usage: nurtuoctl ${cmd.path.join(" ")} ${cmd.usage}`);
  return a;
}

const intFlag = (ctx: Ctx, name: string): number | undefined => {
  const v = str(ctx.p, name);
  if (v === undefined) return undefined;
  if (!/^[0-9]+$/.test(v)) throw new UsageError(`--${name} must be a non-negative integer`);
  return Number(v);
};

async function findComponent(client: NurtuoClient, ref: string): Promise<Component> {
  const list = await client.listComponents();
  const hit = list.find((c) => c.id === ref) ?? list.find((c) => c.name === ref);
  if (!hit) throw new CliError(`component "${ref}" not found`, EXIT.FAILURE, "not_found");
  return hit;
}

function latest(revs: Revision[]): Revision | undefined {
  return revs.reduce<Revision | undefined>((m, r) => (!m || (r.number ?? 0) > (m.number ?? 0) ? r : m), undefined);
}

function printDiagnostics(ctx: Ctx, file: string, diags: Diagnostic[]): void {
  for (const d of diags) ctx.io.stderr(`${file}: ${d.severity}\t${d.code}\t${d.message}\n`);
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

async function waitForDeployment(ctx: Ctx, id: string): Promise<Deployment> {
  const timeoutS = intFlag(ctx, "timeout") ?? 60;
  const interval = intFlag(ctx, "interval") ?? 1000;
  const client = ctx.client();
  const deadline = Date.now() + timeoutS * 1000;
  for (;;) {
    const d = await client.getDeployment(id);
    if (d.status !== "pending") return d;
    if (Date.now() >= deadline) throw new CliError(`timed out after ${timeoutS}s waiting for deployment ${id} (still pending)`, EXIT.FAILURE, "timeout");
    await sleep(interval);
  }
}

const deploymentLine = (d: Deployment) =>
  `deployment ${d.id}  ${d.status}  component=${d.componentId} revision=${d.revisionId} runtime=${d.runtimeId}${d.message ? `  ${d.message}` : ""}`;

const WAIT_FLAGS: FlagSpecs = {
  wait: { type: "boolean", description: "poll until the deployment leaves `pending`; exit 1 if it ends failed/stopped" },
  timeout: { type: "string", value: "<seconds>", description: "with --wait: give up after this many seconds (default 60)" },
  interval: { type: "string", value: "<ms>", description: "with --wait: poll interval (default 1000)" },
};

function finishDeployment(ctx: Ctx, d: Deployment, extra: Record<string, unknown> = {}): number {
  ctx.out({ ...extra, deployment: d }, deploymentLine(d) + "\n");
  return d.status === "failed" || d.status === "stopped" ? EXIT.FAILURE : EXIT.OK;
}

// ------------------------------------------------------------------------------------------------
// commands
// ------------------------------------------------------------------------------------------------

export const COMMANDS: Command[] = [
  {
    path: ["validate"],
    summary: "validate a flow file locally",
    usage: "<file|->",
    details: "Runs the shared flow validator (same rules as the canvas and control plane). Exit 1 if any error is found.",
    examples: ["nurtuoctl validate examples/flows/get-employee.flow.json"],
    async run(ctx) {
      const [file] = needArgs(ctx, this, 1);
      const diags = validateFlow(await readJson(ctx, file!));
      const ok = !hasErrors(diags);
      if (ctx.json) ctx.out({ file, valid: ok, diagnostics: diags }, "");
      else {
        for (const d of diags) ctx.io.stdout(`${d.severity}\t${d.code}\t${d.message}\n`);
        if (!diags.length) ctx.io.stdout(`${file}: ok\n`);
      }
      return ok ? EXIT.OK : EXIT.FAILURE;
    },
  },
  {
    path: ["login"],
    summary: "store the server URL and API token",
    usage: "--server <url> [--token <token> | --token-stdin] [--no-verify]",
    flags: {
      "token-stdin": { type: "boolean", description: "read the token from stdin (keeps it out of shell history)" },
      "no-verify": { type: "boolean", description: "save without calling the server first" },
    },
    details: "Writes the config file with mode 0600 (not enforced on Windows). NURTUO_CONFIG overrides its location. NURTUO_SERVER / NURTUO_TOKEN and the --server / --token flags override stored values per invocation.",
    async run(ctx) {
      needArgs(ctx, this, 0);
      const conn = resolveConnection(ctx.io.env, { server: str(ctx.p, "server"), token: str(ctx.p, "token") });
      if (!conn.server) throw new UsageError("--server <url> is required");
      let server: string;
      try {
        const u = new URL(conn.server);
        if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error("not http(s)");
        server = u.toString().replace(/\/+$/, "");
      } catch {
        throw new UsageError(`invalid server URL "${conn.server}" (expected http:// or https://)`);
      }
      let token = str(ctx.p, "token");
      if (bool(ctx.p, "token-stdin")) token = (await ctx.io.readStdin()).trim();
      token ??= ctx.io.env["NURTUO_TOKEN"] || undefined;
      let verified = false;
      if (!bool(ctx.p, "no-verify")) {
        await new NurtuoClient({ baseUrl: server, ...(token ? { token } : {}), retries: 1 }).listComponents();
        verified = true;
      }
      const path = configPath(ctx.io.env);
      writeConfig(path, { server, ...(token ? { token } : {}) });
      ctx.out({ server, config: path, tokenStored: !!token, verified }, `logged in to ${server}${verified ? "" : " (not verified)"}\nconfig: ${path}\n`);
    },
  },
  {
    path: ["logout"],
    summary: "delete the stored server URL and token",
    usage: "",
    async run(ctx) {
      needArgs(ctx, this, 0);
      const path = configPath(ctx.io.env);
      const removed = removeConfig(path);
      ctx.out({ config: path, removed }, removed ? `removed ${path}\n` : "not logged in\n");
    },
  },
  {
    path: ["component"],
    summary: "manage components (list, get, create, delete, revisions)",
    usage: "<list|get|create|delete|revisions>",
  },
  {
    path: ["component", "list"],
    summary: "list components",
    usage: "",
    async run(ctx) {
      needArgs(ctx, this, 0);
      const items = await ctx.client().listComponents();
      ctx.out({ items }, items.length ? table(items.map((c) => [c.id ?? "", c.name ?? "", c.kind ?? "", c.description ?? ""]), ["ID", "NAME", "KIND", "DESCRIPTION"]) : "no components\n");
    },
  },
  {
    path: ["component", "get"],
    summary: "show one component (by id or name)",
    usage: "<component>",
    async run(ctx) {
      const [ref] = needArgs(ctx, this, 1);
      const c = await findComponent(ctx.client(), ref!);
      ctx.out(c, `id:          ${c.id}\nname:        ${c.name}\nkind:        ${c.kind}\ndescription: ${c.description ?? ""}\ncreated:     ${c.createdAt ?? ""}\n`);
    },
  },
  {
    path: ["component", "create"],
    summary: "create a component",
    usage: "<name> [--kind flow|map|connector|script] [--description <text>]",
    flags: {
      kind: { type: "string", value: "<kind>", description: "component kind (default flow)" },
      description: { type: "string", short: "d", value: "<text>", description: "human description" },
    },
    async run(ctx) {
      const [name] = needArgs(ctx, this, 1);
      const kind = str(ctx.p, "kind");
      const c = await ctx.client().createComponent({
        name: name!,
        ...(kind ? { kind: kind as ComponentKind } : {}),
        ...(str(ctx.p, "description") !== undefined ? { description: str(ctx.p, "description")! } : {}),
      });
      ctx.out(c, `created component ${c.name} (${c.id})\n`);
    },
  },
  {
    path: ["component", "delete"],
    summary: "delete a component (refused while it has revisions)",
    usage: "<component>",
    async run(ctx) {
      const [ref] = needArgs(ctx, this, 1);
      const client = ctx.client();
      const c = await findComponent(client, ref!);
      await client.deleteComponent(c.id!);
      ctx.out({ deleted: c.id, name: c.name }, `deleted component ${c.name} (${c.id})\n`);
    },
  },
  {
    path: ["component", "revisions"],
    summary: "list the revisions of a component",
    usage: "<component>",
    async run(ctx) {
      const [ref] = needArgs(ctx, this, 1);
      const client = ctx.client();
      const c = await findComponent(client, ref!);
      const items = await client.listRevisions(c.id!);
      const slim = items.map(({ content: _c, ...r }) => r);
      ctx.out({ component: c, items: slim }, items.length ? table(slim.map((r) => [String(r.number), r.id ?? "", r.digest ?? "", r.createdAt ?? ""]), ["REV", "ID", "DIGEST", "CREATED"]) : "no revisions\n");
    },
  },
  {
    path: ["flow"],
    summary: "push and pull flow documents",
    usage: "<push|pull>",
  },
  {
    path: ["flow", "push"],
    summary: "validate a flow locally, then store it as a new revision",
    usage: "<file|-> [--component <name|id>] [--create] [--force]",
    flags: {
      component: { type: "string", value: "<name|id>", description: "target component (default: the flow's id)" },
      create: { type: "boolean", description: "create the component if it does not exist" },
      force: { type: "boolean", description: "push even if the latest revision has identical content" },
    },
    details: "Validation errors abort the push (exit 1) before any request is sent; warnings are printed to stderr. If the latest revision already has identical content (key order ignored) nothing is pushed unless --force.",
    async run(ctx) {
      const [file] = needArgs(ctx, this, 1);
      const flow = await readJson(ctx, file!);
      const diags = validateFlow(flow);
      if (hasErrors(diags)) {
        if (ctx.json) ctx.out({ file, pushed: false, valid: false, diagnostics: diags }, "");
        else {
          printDiagnostics(ctx, file!, diags);
          ctx.io.stderr(`${file}: not pushed: flow has validation errors\n`);
        }
        return EXIT.FAILURE;
      }
      if (!ctx.json) printDiagnostics(ctx, file!, diags);
      const client = ctx.client();
      const ref = str(ctx.p, "component") ?? String((flow as { id: string }).id);
      let comp: Component;
      try {
        comp = await findComponent(client, ref);
      } catch (e) {
        if (!(e instanceof CliError) || e.code !== "not_found") throw e;
        if (!bool(ctx.p, "create")) throw new CliError(`component "${ref}" not found (use --create to create it, or --component <name|id>)`, EXIT.FAILURE, "not_found");
        const description = (flow as { description?: string }).description;
        comp = await client.createComponent({ name: ref, kind: "flow", ...(description ? { description } : {}) });
      }
      const prev = latest(await client.listRevisions(comp.id!));
      if (prev && !bool(ctx.p, "force") && canonicalJson(prev.content) === canonicalJson(flow)) {
        ctx.out({ component: comp.name, revision: prev.number, id: prev.id, digest: prev.digest, created: false }, `unchanged: ${comp.name} already at revision ${prev.number}\n`);
        return;
      }
      const rev = await client.createRevision(comp.id!, flow as object);
      ctx.out({ component: comp.name, revision: rev.number, id: rev.id, digest: rev.digest, created: true }, `pushed ${comp.name} revision ${rev.number} (${rev.digest})\n`);
    },
  },
  {
    path: ["flow", "pull"],
    summary: "fetch a flow revision (latest by default)",
    usage: "<component> [--revision <n>] [-o <file>]",
    flags: {
      revision: { type: "string", short: "r", value: "<n>", description: "revision number (default: latest)" },
      output: { type: "string", short: "o", value: "<file>", description: "write to a file instead of stdout" },
    },
    details: "Without -o the canonical flow JSON is written to stdout (also with --json). With -o and --json a small metadata object is printed instead.",
    async run(ctx) {
      const [ref] = needArgs(ctx, this, 1);
      const client = ctx.client();
      const comp = await findComponent(client, ref!);
      const n = intFlag(ctx, "revision");
      let rev: Revision | undefined;
      if (n !== undefined) {
        if (n < 1) throw new UsageError("--revision must be >= 1");
        rev = await client.getRevision(comp.id!, n);
      } else {
        rev = latest(await client.listRevisions(comp.id!));
        if (!rev) throw new CliError(`component "${comp.name}" has no revisions`, EXIT.FAILURE, "not_found");
      }
      const text = formatFlow(rev.content);
      const outFile = str(ctx.p, "output");
      if (outFile) {
        writeFileSync(outFile, text);
        ctx.out({ component: comp.name, revision: rev.number, digest: rev.digest, file: outFile }, `wrote ${comp.name} revision ${rev.number} to ${outFile}\n`);
      } else ctx.io.stdout(text);
    },
  },
  {
    path: ["deploy"],
    summary: "deploy a component revision to a runtime, or check a deployment",
    usage: "<component> --runtime <id> [--revision <n>] [--wait]   |   status <deployment-id> [--wait]",
    flags: {
      runtime: { type: "string", value: "<id>", description: "target runtime id" },
      revision: { type: "string", short: "r", value: "<n>", description: "revision number (default: latest)" },
      ...WAIT_FLAGS,
    },
    details: "Records the desired state for the runtime; the runtime converges asynchronously, so the returned deployment is normally `pending`. Use --wait or `deploy status` to follow it. A component literally named \"status\" can be deployed by id.",
    examples: ["nurtuoctl deploy orders --runtime rt-dev-1 --revision 3 --wait", "nurtuoctl deploy status dep_1a2b3c"],
    async run(ctx) {
      const [ref] = needArgs(ctx, this, 1);
      const runtimeId = str(ctx.p, "runtime");
      if (!runtimeId) throw new UsageError("--runtime <id> is required");
      const client = ctx.client();
      const comp = await findComponent(client, ref!);
      let number = intFlag(ctx, "revision");
      if (number === undefined) {
        const rev = latest(await client.listRevisions(comp.id!));
        if (!rev) throw new CliError(`component "${comp.name}" has no revisions; run \`nurtuoctl flow push\` first`, EXIT.FAILURE, "not_found");
        number = rev.number!;
      } else if (number < 1) throw new UsageError("--revision must be >= 1");
      const res = await client.deploy({ componentId: comp.id!, runtimeId, revision: number });
      const d = bool(ctx.p, "wait") ? await waitForDeployment(ctx, res.deployment.id!) : res.deployment;
      return finishDeployment(ctx, d, { revision: number, generation: res.desiredState.generation });
    },
  },
  {
    path: ["deploy", "status"],
    summary: "show the status of a deployment",
    usage: "<deployment-id> [--wait] [--timeout <s>]",
    flags: WAIT_FLAGS,
    details: "Exit 0 for pending/running, 1 for failed/stopped. With --wait, polls until the deployment leaves `pending`.",
    async run(ctx) {
      const [id] = needArgs(ctx, this, 1);
      const d = bool(ctx.p, "wait") ? await waitForDeployment(ctx, id!) : await ctx.client().getDeployment(id!);
      return finishDeployment(ctx, d);
    },
  },
  {
    path: ["runtime"],
    summary: "inspect runtimes",
    usage: "<list>",
  },
  {
    path: ["runtime", "list"],
    summary: "list registered runtimes",
    usage: "[--environment <name>]",
    flags: { environment: { type: "string", short: "e", value: "<name>", description: "only runtimes in this environment" } },
    async run(ctx) {
      needArgs(ctx, this, 0);
      const env = str(ctx.p, "environment");
      const items = await ctx.client().listRuntimes(env ? { environment: env } : {});
      ctx.out({ items }, items.length ? table(items.map((r) => [r.id ?? "", r.name ?? "", r.environment ?? "", r.status ?? "", r.version ?? "", r.lastSeenAt ?? ""]), ["ID", "NAME", "ENVIRONMENT", "STATUS", "VERSION", "LAST SEEN"]) : "no runtimes\n");
    },
  },
  {
    path: ["diff"],
    summary: "semantic diff of two flow files (by step id and edge)",
    usage: "<a.json> <b.json> [--exit-code]",
    flags: { "exit-code": { type: "boolean", description: "exit 1 when the flows differ (like git diff --exit-code)" } },
    details: "Compares steps by id (config compared field by field) and edges by `step:port` key. Step order and JSON key order are ignored. Exit 0 even when different unless --exit-code is given.",
    async run(ctx) {
      const [a, b] = needArgs(ctx, this, 2);
      if (a === "-" && b === "-") throw new UsageError("only one side can be read from stdin");
      const d = diffFlows(await readJson(ctx, a!), await readJson(ctx, b!));
      ctx.out(d, renderDiff(d));
      return bool(ctx.p, "exit-code") && !d.identical ? EXIT.FAILURE : EXIT.OK;
    },
  },
  {
    path: ["fmt"],
    summary: "canonically format a flow file",
    usage: "<file|-> [-w] [--check]",
    flags: {
      write: { type: "boolean", short: "w", description: "rewrite the file in place" },
      check: { type: "boolean", description: "exit 1 if the file is not canonically formatted; write nothing" },
    },
    details: "Canonical form: fixed key order (schema, id, name, description, revision, steps, edges), sorted step config keys, edges sorted by key, steps in authored order, 2-space indent, trailing newline. Without -w/--check the result goes to stdout.",
    async run(ctx) {
      const [file] = needArgs(ctx, this, 1);
      const raw = await readInput(ctx, file!);
      let doc: unknown;
      try {
        doc = JSON.parse(raw);
      } catch (e) {
        throw new CliError(`${file}: invalid JSON: ${(e as Error).message}`, EXIT.FAILURE, "invalid_json");
      }
      const text = formatFlow(doc);
      const same = text === raw.replace(/\r\n/g, "\n");
      if (bool(ctx.p, "check")) {
        ctx.out({ file, formatted: same }, same ? `${file}: formatted\n` : `${file}: not formatted\n`);
        return same ? EXIT.OK : EXIT.FAILURE;
      }
      if (bool(ctx.p, "write")) {
        if (file === "-") throw new UsageError("cannot use -w with stdin");
        if (!same) writeFileSync(file!, text);
        ctx.out({ file, changed: !same }, same ? `${file}: already formatted\n` : `formatted ${file}\n`);
      } else ctx.io.stdout(text);
    },
  },
  {
    path: ["init"],
    summary: "scaffold a new flow file",
    usage: "<name> [--template minimal|http-get] [-o <file>] [--force]",
    flags: {
      template: { type: "string", short: "t", value: "<name>", description: `template: ${Object.keys(TEMPLATES).join(", ")} (default minimal)` },
      output: { type: "string", short: "o", value: "<file>", description: "output path (default <id>.flow.json)" },
      force: { type: "boolean", description: "overwrite an existing file" },
    },
    details: Object.entries(TEMPLATES).map(([k, v]) => `  ${k.padEnd(10)} ${v}`).join("\n"),
    async run(ctx) {
      const [name] = needArgs(ctx, this, 1);
      const id = slugify(name!);
      if (!id) throw new UsageError(`cannot derive a flow id from "${name}"`);
      const t = (str(ctx.p, "template") ?? "minimal") as TemplateName;
      if (!(t in TEMPLATES)) throw new UsageError(`unknown template "${t}"; available: ${Object.keys(TEMPLATES).join(", ")}`);
      const file = str(ctx.p, "output") ?? `${id}.flow.json`;
      if (existsSync(file) && !bool(ctx.p, "force")) throw new CliError(`${file} already exists (use --force to overwrite)`, EXIT.FAILURE, "exists");
      writeFileSync(file, formatFlow(scaffold(name!, t)));
      ctx.out({ file, id, template: t }, `created ${file} (template ${t})\nnext: nurtuoctl validate ${file}\n`);
    },
  },
  {
    path: ["completion"],
    summary: "print a shell completion script",
    usage: "<bash|zsh|powershell>",
    examples: ["source <(nurtuoctl completion bash)", "nurtuoctl completion powershell | Out-String | Invoke-Expression"],
    async run(ctx) {
      const [shell] = needArgs(ctx, this, 1);
      if (!SHELLS.includes(shell as Shell)) throw new UsageError(`unknown shell "${shell}"; expected ${SHELLS.join(", ")}`);
      ctx.io.stdout(generateCompletion(shell as Shell, completionTree()));
    },
  },
];

export function completionTree(): TreeNode[] {
  const flagWords = (c: Command) => Object.entries({ ...GLOBAL_FLAGS, ...c.flags }).flatMap(([n, s]) => [`--${n}`, ...(s.short ? [`-${s.short}`] : [])]);
  const tops = [...new Set(COMMANDS.map((c) => c.path[0]!))];
  const tree: TreeNode[] = [{ path: "", words: [...tops, "help", "--help", "--version", "--json"] }];
  for (const c of COMMANDS) {
    const subs = COMMANDS.filter((s) => s.path.length === c.path.length + 1 && s.path.slice(0, c.path.length).join(" ") === c.path.join(" ")).map((s) => s.path[c.path.length]!);
    tree.push({ path: c.path.join(" "), words: [...subs, ...flagWords(c)] });
  }
  return tree;
}

// ------------------------------------------------------------------------------------------------
// help
// ------------------------------------------------------------------------------------------------

function flagLine(name: string, s: FlagSpecs[string]): string {
  const left = `${s.short ? `-${s.short}, ` : "    "}--${name}${s.value ? ` ${s.value}` : ""}`;
  return `  ${left.padEnd(30)} ${s.description}`;
}

export function rootHelp(): string {
  const groups = COMMANDS.filter((c) => c.path.length === 1);
  const w = Math.max(...groups.map((c) => c.path[0]!.length)) + 2;
  return `nurtuoctl: validate, diff, push and deploy Nurtuo flows from the terminal and CI

Usage: nurtuoctl <command> [arguments] [flags]

Commands:
${groups.map((c) => `  ${c.path[0]!.padEnd(w)}${c.summary}`).join("\n")}
  ${"help".padEnd(w)}show help for a command

Global flags:
${Object.entries(GLOBAL_FLAGS).map(([n, s]) => flagLine(n, s)).join("\n")}
      --version                  print the version

Environment: NURTUO_SERVER, NURTUO_TOKEN, NURTUO_CONFIG
Exit codes: 0 ok, 1 failure (validation/API error), 2 usage error, 3 network error, 4 auth error
Run \`nurtuoctl <command> --help\` for details.
`;
}

export function commandHelp(c: Command): string {
  const subs = COMMANDS.filter((s) => s.path.length === c.path.length + 1 && s.path.slice(0, c.path.length).join(" ") === c.path.join(" "));
  const lines = [`nurtuoctl ${c.path.join(" ")}: ${c.summary}`, "", `Usage: nurtuoctl ${c.path.join(" ")} ${c.usage}`.trimEnd()];
  if (c.details) lines.push("", c.details);
  if (subs.length) {
    lines.push("", "Subcommands:");
    for (const s of subs) lines.push(`  ${s.path[s.path.length - 1]!.padEnd(10)} ${s.summary}`);
  }
  const flags = { ...(c.flags ?? {}), ...GLOBAL_FLAGS };
  lines.push("", "Flags:", ...Object.entries(flags).map(([n, s]) => flagLine(n, s)));
  if (c.examples?.length) lines.push("", "Examples:", ...c.examples.map((e) => `  ${e}`));
  return lines.join("\n") + "\n";
}

// ------------------------------------------------------------------------------------------------
// dispatch
// ------------------------------------------------------------------------------------------------

/** Pulls command words off the front of argv, skipping global flags (and their values) before them. */
function splitCommand(argv: readonly string[]): { words: string[]; rest: string[] } {
  const words: string[] = [];
  const rest: string[] = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]!;
    if (a.startsWith("-") && a !== "-") {
      rest.push(a);
      if ((a === "--server" || a === "--token") && i + 1 < argv.length) rest.push(argv[++i]!);
    } else if (words.length < 2 && !rest.some((r) => r === "--") && (words.length === 0 || COMMANDS.some((c) => c.path[0] === words[0] && c.path.length > 1))) {
      words.push(a);
    } else rest.push(a);
  }
  return { words, rest };
}

function version(): string {
  try {
    return (JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as { version: string }).version;
  } catch {
    return "unknown";
  }
}

export async function run(argv: readonly string[], io: Io): Promise<number> {
  const jsonRequested = argv.includes("--json") || argv.includes("--json=true");
  const fail = (e: unknown): number => {
    let exit: number = EXIT.FAILURE;
    let code = "error";
    let message = e instanceof Error ? e.message : String(e);
    if (e instanceof UsageError) { exit = EXIT.USAGE; code = "usage"; message += "\nrun with --help for usage"; }
    else if (e instanceof CliError) { exit = e.exit; code = e.code; }
    else if (e instanceof ApiError) {
      exit = e.status === 401 || e.status === 403 ? EXIT.AUTH : EXIT.FAILURE;
      code = e.code;
      message = `${e.message} (HTTP ${e.status})`;
      if (exit === EXIT.AUTH) message += "; check your token (nurtuoctl login)";
    } else if (e instanceof NetworkError) { exit = EXIT.NETWORK; code = e.timedOut ? "timeout" : "network"; }
    if (jsonRequested) io.stderr(JSON.stringify({ error: { code, message } }) + "\n");
    else io.stderr(`nurtuoctl: ${message}\n`);
    return exit;
  };

  try {
    if (argv.length === 0) { io.stdout(rootHelp()); return EXIT.OK; }
    if (argv.includes("--version") && !argv.includes("--")) { io.stdout(version() + "\n"); return EXIT.OK; }
    const { words, rest } = splitCommand(argv);
    if (words[0] === "help") {
      const target = COMMANDS.find((c) => c.path.join(" ") === words.slice(1).concat(rest.filter((r) => !r.startsWith("-"))).join(" "));
      if (words.length === 1 && !rest.some((r) => !r.startsWith("-"))) { io.stdout(rootHelp()); return EXIT.OK; }
      if (!target) throw new UsageError(`unknown command "${[...words.slice(1), ...rest.filter((r) => !r.startsWith("-"))].join(" ")}"`);
      io.stdout(commandHelp(target));
      return EXIT.OK;
    }
    if (rest.includes("--help") || rest.includes("-h")) {
      if (!words.length) { io.stdout(rootHelp()); return EXIT.OK; }
    }
    // longest registered command path that matches the leading words
    let cmd = COMMANDS.find((c) => c.path.length === 2 && c.path[0] === words[0] && c.path[1] === words[1]);
    let consumed = cmd ? 2 : 0;
    if (!cmd) {
      cmd = COMMANDS.find((c) => c.path.length === 1 && c.path[0] === words[0]);
      consumed = cmd ? 1 : 0;
    }
    if (!cmd) throw new UsageError(`unknown command "${words[0]}"`);
    const positionals = words.slice(consumed);
    const p = parseArgs(rest, { ...GLOBAL_FLAGS, ...(cmd.flags ?? {}) });
    p.positionals.unshift(...positionals);
    if (bool(p, "help")) { io.stdout(commandHelp(cmd)); return EXIT.OK; }
    if (!cmd.run) {
      io.stderr(commandHelp(cmd));
      throw new UsageError(positionals.length ? `unknown subcommand "${positionals[0]}" for "${cmd.path.join(" ")}"` : `"${cmd.path.join(" ")}" needs a subcommand`);
    }

    const json = bool(p, "json");
    const ctx: Ctx = {
      io,
      p,
      json,
      client() {
        const conn = resolveConnection(io.env, { server: str(p, "server"), token: str(p, "token") });
        if (!conn.server) throw new CliError("no server configured; run `nurtuoctl login --server <url>` or set NURTUO_SERVER", EXIT.USAGE, "no_server");
        return new NurtuoClient({ baseUrl: conn.server, ...(conn.token ? { token: conn.token } : {}) });
      },
      out(data, text) {
        if (json) io.stdout(JSON.stringify(data, null, 2) + "\n");
        else if (text) io.stdout(text);
      },
      info: (s) => io.stderr(s),
    };
    const code = await cmd.run(ctx);
    return code ?? EXIT.OK;
  } catch (e) {
    return fail(e);
  }
}
