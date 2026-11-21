# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/nurtuo/nurtuo/security/advisories/new).
Do not open a public issue.

We aim to acknowledge reports within 3 business days and to publish a fix and advisory
within 90 days, coordinating disclosure with the reporter.

## Supported versions

Until 1.0, only the latest release receives security fixes. After 1.0, the latest minor
release and the previous one are supported.

## Release integrity

Release binaries and bridge artifacts are signed with Sigstore cosign (keyless) and ship with
an SPDX SBOM. Verify before installing; the runtime verifies bridge signatures before starting them.
