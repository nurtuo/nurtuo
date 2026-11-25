// SPDX-License-Identifier: AGPL-3.0-only
import { createHash, createPrivateKey, createPublicKey, sign, type KeyObject } from "node:crypto";
import { readFileSync } from "node:fs";

export const bundleNamePattern = /^[a-z0-9][a-z0-9._-]{0,127}$/;
export const maxBundleSize = 256 * 1024 * 1024;
export const maxBundleEntrySize = 64 * 1024 * 1024;
const blockSize = 512;
const signatureSize = 64;
const signingKeyEnv = "NURTUO_BUNDLE_SIGNING_KEY_FILE";

export interface SigningKey {
  privateKey: KeyObject;
  keyId: string;
}

interface BundleInput {
  name: string;
  createdAt: string;
  component: { kind: string; id: string; revision: number; document: unknown };
}

const sha256 = (data: Buffer | string) => createHash("sha256").update(data).digest("hex");

export function loadSigningKey(path: string): SigningKey {
  let seed: string;
  try {
    seed = readFileSync(path, "utf8").trim();
  } catch {
    throw new Error(`${signingKeyEnv}: cannot read signing key file`);
  }
  if (!/^[0-9a-fA-F]{64}$/.test(seed)) {
    throw new Error(`${signingKeyEnv}: expected a 32-byte hex Ed25519 seed`);
  }
  try {
    // RFC 8410 PKCS#8 prefix for an Ed25519 seed.
    const der = Buffer.from(`302e020100300506032b657004220420${seed}`, "hex");
    const privateKey = createPrivateKey({ key: der, format: "der", type: "pkcs8" });
    const publicKey = createPublicKey(privateKey).export({ format: "der", type: "spki" }).subarray(-32);
    return { privateKey, keyId: sha256(publicKey).slice(0, 16) };
  } catch {
    throw new Error(`${signingKeyEnv}: cannot import Ed25519 signing key`);
  }
}

/** The regular-file USTAR header used by Go archive/tar. Paths are ASCII. */
function writeHeader(header: Buffer, path: string, size: number): void {
  let name = path;
  if (path.length > 100) {
    const split = path.lastIndexOf("/", 155);
    if (split <= 0 || path.length - split - 1 > 100) {
      throw new Error("bundle: component path does not fit USTAR");
    }
    name = path.slice(split + 1);
    header.write(path.slice(0, split), 345, 155, "ascii");
  }
  header.write(name, 0, 100, "ascii");

  const octal = (value: number, offset: number, length: number) => {
    header.write(value.toString(8).padStart(length - 1, "0"), offset, length - 1, "ascii");
  };
  octal(0o644, 100, 8);
  octal(0, 108, 8);
  octal(0, 116, 8);
  octal(size, 124, 12);
  octal(0, 136, 12);
  header.fill(0x20, 148, 156);
  header.write("0", 156, "ascii");
  header.write("ustar\0" + "00", 257, "ascii");
  octal(0, 329, 8);
  octal(0, 337, 8);

  const checksum = header.reduce((sum, byte) => sum + byte, 0);
  octal(checksum, 148, 7);
  header[154] = 0;
}

const paddedSize = (size: number) => Math.ceil(size / blockSize) * blockSize;

const wellFormed = (value: string) => value.replace(/[\uD800-\uDFFF]/gu, "\uFFFD");
const quote = (value: string) => JSON.stringify(wellFormed(value)).replace(/\u2028/g, "\\u2028").replace(/\u2029/g, "\\u2029");

function canonicalBundleJson(value: unknown): string {
  if (typeof value === "string") return quote(value);
  if (typeof value === "number" && !Number.isFinite(value)) throw new Error("bundle: non-finite number");
  if (Array.isArray(value)) return `[${value.map(canonicalBundleJson).join(",")}]`;
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value).map(([key, item]) => ({ key: wellFormed(key), item }));
    if (new Set(entries.map(({ key }) => key)).size !== entries.length) {
      throw new Error("bundle: duplicate JSON key");
    }
    const sorted = entries.map((entry) => ({ ...entry, bytes: Buffer.from(entry.key) }))
      .sort((a, b) => a.bytes.compare(b.bytes));
    return `{${sorted.map(({ key, item }) => `${quote(key)}:${canonicalBundleJson(item)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

/** Pack one deployed component into a deterministic, signed nurtuo.bundle/v1 archive. */
export function packBundle(input: BundleInput, key: SigningKey): Buffer {
  const { kind, id, revision, document } = input.component;
  if (!bundleNamePattern.test(input.name)) throw new Error("bundle: invalid name");
  if (!bundleNamePattern.test(kind) || !bundleNamePattern.test(id) || !Number.isSafeInteger(revision) || revision < 1) {
    throw new Error("bundle: invalid component kind, id or revision");
  }
  if (document === null || typeof document !== "object" || Array.isArray(document)) {
    throw new Error("bundle: component document must be a JSON object");
  }
  const createdAt = new Date(input.createdAt).toISOString().replace(/\.\d{3}Z$/, "Z");
  const documentJson = canonicalBundleJson(document);
  const documentSize = Buffer.byteLength(documentJson);
  if (documentSize > maxBundleEntrySize) throw new Error(`bundle: entry exceeds ${maxBundleEntrySize} bytes`);
  const config = "{}";
  const path = `components/${kind}/${id}/${revision}.json`;
  const manifest = JSON.stringify({
    format: "nurtuo.bundle/v1",
    name: input.name,
    createdAt,
    components: [{ kind, id, revision, path, sha256: sha256(documentJson), size: documentSize }],
    config: { path: "config.json", sha256: sha256(config), size: config.length },
    connectors: [],
  });
  const signatureDoc = (sig: string) => JSON.stringify({ alg: "ed25519", keyId: key.keyId, sig });
  const entries = [
    { path: "manifest.json", data: manifest, size: Buffer.byteLength(manifest) },
    { path: "config.json", data: config, size: config.length },
    { path, data: documentJson, size: documentSize },
    { path: "manifest.sig", data: "", size: Buffer.byteLength(signatureDoc("A".repeat(Math.ceil(signatureSize / 3) * 4))) },
  ];
  if (entries.some((entry) => entry.size > maxBundleEntrySize)) {
    throw new Error(`bundle: entry exceeds ${maxBundleEntrySize} bytes`);
  }
  const size = entries.reduce((total, entry) => total + blockSize + paddedSize(entry.size), 2 * blockSize);
  if (size > maxBundleSize) throw new Error(`bundle: archive exceeds ${maxBundleSize} bytes`);

  const signature = sign(null, Buffer.from(`nurtuo-bundle-v1\n${manifest}`), key.privateKey);
  entries[3]!.data = signatureDoc(signature.toString("base64"));
  const archive = Buffer.alloc(size);
  let offset = 0;
  for (const entry of entries) {
    writeHeader(archive.subarray(offset, offset + blockSize), entry.path, entry.size);
    archive.write(entry.data, offset + blockSize, entry.size);
    offset += blockSize + paddedSize(entry.size);
  }
  return archive;
}
