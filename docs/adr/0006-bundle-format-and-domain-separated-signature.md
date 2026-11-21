<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0006: Bundle format `nurtuo.bundle/v1`, signed with Ed25519 over a domain-separated manifest

Date: 2026-11-21. Status: accepted (package `core/deploy/bundle`; runtime integration pending).

**Context.** The control plane ships deployments to runtimes that sit in customer networks and
dial out. A runtime must be able to tell that a deployment is exactly what the control plane
signed. Reproducible output makes change detection and audits easier. Hand-rolled archive parsing is
a classic source of vulnerabilities (path traversal, ambiguous entries, trailing data).

**Decision.**

- A bundle is an uncompressed USTAR tar with zeroed metadata and entries in a fixed order:
  `manifest.json`, `config.json`, `components/<kind>/<id>/<revision>.json` (sorted), `manifest.sig`.
- JSON documents are re-encoded as canonical JSON (sorted keys, no insignificant whitespace, number
  literals preserved); duplicate object keys are rejected. No clock or randomness is read, so the
  same input and key produce a byte-identical archive.
- The manifest pins every file by SHA-256 and size and lists components and connector versions
  (sorted, strictly, so duplicates are impossible).
- The signature is Ed25519 over `"nurtuo-bundle-v1\n" + manifest.json`. The fixed prefix is domain
  separation: the same key can sign other kinds of messages without a signature being valid in two
  contexts, and the version in the prefix ties it to this format. `manifest.sig` is
  `{"alg":"ed25519","keyId","sig"}` with `keyId` the first 16 hex characters of the SHA-256 of the
  public key.
- `Verify` checks the signature first, then canonical entry order, hashes, shapes and size limits
  (256 MiB default), and rejects any trailing bytes so the container is non-malleable.

**Consequences.**

- Tampering with the manifest, any file, the order or the container is detected; the format is
  stable for diffing and caching.
- Tar header metadata is unsigned and ignored; the archive hash is not an identity (identify a
  bundle by signing key id plus manifest hash).
- Key distribution, rotation, revocation and trust stores are explicitly out of scope of this
  package. Until the runtime calls `Verify`, the format gives no protection in a running system
  (see [security.md](../security.md)).
- A future incompatible format needs `nurtuo.bundle/v2` and a new domain prefix.
