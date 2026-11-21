<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# bundle (deployment bundle format)

Go package `github.com/nurtuo/nurtuo/core/deploy/bundle`, stdlib only. The control plane packs and
signs; the runtime verifies before use. Flow and component documents are opaque JSON.

## Format `nurtuo.bundle/v1`

A bundle is an uncompressed USTAR tar with zeroed metadata (mtime 0, mode 0644, no owners) and
entries in this exact order:

1. `manifest.json`
2. `config.json` (deploy-time config, canonical JSON)
3. `components/<kind>/<id>/<revision>.json`, sorted by path (canonical JSON)
4. `manifest.sig` (detached signature, always last)

`manifest.json` holds `format`, `name`, `createdAt` (caller supplied, RFC 3339 UTC), `components`
(kind, id, revision, path, sha256, size), `config` (path, sha256, size) and `connectors` (id,
version, optional registry digest), all sorted. `manifest.sig` is JSON
`{"alg":"ed25519","keyId":"<16 hex>","sig":"<base64>"}`; the signature covers
`"nurtuo-bundle-v1\n" + manifest.json bytes`, and the manifest pins every other file by sha256.

## Determinism

Documents are re-encoded as canonical JSON (sorted keys, no whitespace, number literals kept),
collections are sorted, no clock or randomness is read, and Ed25519 signatures are deterministic.
Same logical input and key give a byte-identical bundle (useful for reproducible builds and
change detection; see the security notes before using the archive hash as an identity).

## Verification (`Verify`)

Rejected: missing signature (`ErrMissingSig`), wrong key or edited manifest (`ErrBadSignature`),
edited file (`ErrHashMismatch`), reordered, extra or missing entries (`ErrNonCanonical`),
duplicate entries, non-regular entries, malformed archives (`ErrInvalid`). The signature is checked
before the manifest is parsed for anything else.

Also rejected: any bytes after the tar end-of-archive marker (the container is non-malleable),
a config path other than `config.json`, component paths that do not match their kind/id/revision,
manifest `components`/`connectors` that are not strictly sorted (so duplicates are rejected), and
archives or contents larger than `MaxBundleSize` (default 256 MiB, a package variable; checked
before entries are buffered). `Pack` rejects duplicate connector refs and oversize output.

## Security notes

- Duplicate JSON object keys: `encoding/json` would keep only the last value, so two
  semantically different documents could share a reading. Canonicalisation therefore rejects
  duplicate keys at any depth (`ErrInvalid`) instead of collapsing them.
- Tar header metadata (mtime, mode, uid/gid, names of owners) is not signed. Only the entry name,
  order and content are covered (via the manifest hashes). Verify ignores the rest; do not
  rely on it.
- Do not use the archive hash as an identity for a bundle: the signature is over the manifest,
  and although Verify enforces a canonical container, the hash of the raw archive is not a
  signed value. Identify a bundle by its verified manifest (signing key id plus manifest hash).

## Keys

`GenerateKey`, `EncodePrivateKey`/`DecodePrivateKey` (hex seed), `EncodePublicKey`/`DecodePublicKey`
(hex), `KeyID`. Key distribution, rotation and trust-store handling are out of scope here.

Tests: `GOWORK=off go test ./...` from this directory (not yet part of `go.work`).
