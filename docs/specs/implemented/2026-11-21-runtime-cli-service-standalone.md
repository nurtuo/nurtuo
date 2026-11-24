<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: `nurtuo` command, OS service and standalone mode

Status: implemented (`role` is a stub). Date: 2026-11-21 (written as built on 2026-11-22).

## TLDR

`nurtuo` is one static Go binary with the subcommands `run`, `service`, `standalone`, `secret`,
`role` (stub) and `version`. `service` wraps `kardianos/service` (Windows service, systemd,
launchd); `standalone` bootstraps a local CA and secrets, starts an embedded PostgreSQL, the
control plane as a supervised Node child and the runtime against it. As built, the standalone
runtime could not complete the gateway handshake (gap 1, fixed in #56); it still has no
`bundle_dir` (gap 2).

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/25 (squash commit `ee4d733`), https://github.com/nurtuo/nurtuo/pull/16 (release dry-run, squash commit `3d3d506`) |
| ADRs | [ADR 0009](../../adr/0009-outbound-only-runtime-gateway.md), [ADR 0010](../../adr/0010-connections-and-secrets.md) |
| Code | `core/runtime/cmd/nurtuo/{main.go,run.go,service.go,service_goos.go,secret.go,standalone*.go}`, `core/deploy/systemd/nurtuo.service`, `.goreleaser.yaml`, `Makefile` (`snapshot`), `.github/workflows/release-snapshot.yml` |
| User docs | [operations.md](../../operations.md), [ARCHITECTURE.md](../../ARCHITECTURE.md) |

## Problem and goals

Operators install the runtime on Windows servers, Linux VMs and Macs inside customer networks.
Evaluators want one command that brings up the whole product on a laptop.

- Goals: one binary per OS/arch, no runtime dependencies (`CGO_ENABLED=0`); native service
  integration with restart on failure and OS logging; a zero-configuration local stack with
  generated credentials.
- Non-goals (as built): distributed roles (`nurtuo role`), installers (MSI, notarized pkg),
  container images, certificate renewal, packaging of the control plane.

## Design (as built)

**Dispatch** (`main.go`): subcommands register themselves with `register(name, fn)` from `init()`;
`version` prints `nurtuo <Version> (<Commit>, <Date>)` (ldflags from `.goreleaser.yaml`, `dev (none,
unknown)` locally); `role` is in `stubs` and prints `nurtuo role: not implemented yet`, exit 1;
anything else prints usage, exit 2.

**`run`** (`run.go`): `--config` (default `defaultConfigPath`: `%ProgramData%\Nurtuo\runtime.yaml`,
`/Library/Application Support/Nurtuo/runtime.yaml`, `/etc/nurtuo/runtime.yaml`),
`runtime.LoadConfig`, `signal.NotifyContext(os.Interrupt, SIGTERM)`, `slog` text to stderr,
`runtime.RunWith(ctx, cfg, runOptions(logger))` with `connhost.Builtins()`.

**`service <install|uninstall|start|stop|status|run> [--config P]`** (`service.go`): service name
`nurtuo`, display name "Nurtuo Runtime", arguments `service run --config <abs path>`, options
`Restart=on-failure` (systemd), `KeepAlive`/`RunAtLoad` (launchd), `OnFailure=restart` with 5 s
delay (Windows). `install` refuses an invalid config. `run` (invoked by the service manager) loads
the config, forwards `slog` lines to the OS logger when not interactive, and runs the runtime in
`program.Start`'s goroutine; `Stop` cancels and waits up to 20 s (`stopTimeout`). No service
account is configured, so the service runs as the service manager's default account (LocalSystem
on Windows, root for system units). The deb/rpm template `core/deploy/systemd/nurtuo.service`
instead runs `nurtuo run --config /etc/nurtuo/runtime.yaml` as `User=nurtuo`, which the package
does not create.

**`standalone`** (`standalone*.go`): flags `--data-dir` (default `DefaultDataRoot`:
`NURTUO_DATA_DIR`, else `%ProgramData%\Nurtuo` / `%LOCALAPPDATA%\Nurtuo`, `~/Library/Application
Support/Nurtuo`, `$XDG_DATA_HOME/nurtuo` / `~/.local/share/nurtuo`), `--port` 8080,
`--control-plane-cmd` `node`, `--control-plane-entry` (resolved by `resolveControlPlaneEntry`),
`--gateway-addr` `127.0.0.1:8443`, `--dev` (ignored). `Stack.Run` orders:

1. `DataDirs.Ensure` (`secrets/` 0700, others 0755).
2. `localBootstrapper.Bootstrap`: `db-password`, `admin-token` (24 random bytes hex, 0600); if any
   certificate is missing, a new ECDSA P-256 CA (10 years) and leaf certificates (2 years) for
   `nurtuo-control-plane` (server auth) and `nurtuo-runtime` (client auth), SANs `localhost`,
   `127.0.0.1`, `::1`. The CA private key is not written to disk.
3. `embeddedDB.Start` (`fergusstrange/embedded-postgres`, downloads binaries on first start).
4. Control plane child (`NewChild`, `MaxRestarts: 10`, exponential backoff, a run of at least
   `StableFor` resets it) with `controlPlaneEnv`: `DATABASE_URL`, `PORT`, `GATEWAY_PORT`,
   `GATEWAY_CERT/KEY/CA`, `NURTUO_AUTH_SECRET` and `NURTUO_BOOTSTRAP_PASSWORD` both set to the
   admin token, `NURTUO_BOOTSTRAP_EMAIL=admin@nurtuo.local`; `WaitHealthy` polls `GET /`.
5. Runtime: `runRealRuntime` calls `runtime.Run` with `RuntimeID: "standalone"`, the client
   certificate, `ServerName: "localhost"` and secrets `encrypted-file` at `secrets/store`; no
   `BundleDir`.

Shutdown on `os.Interrupt` only: runtime, control plane, database (`TestShutdownOrdering`).

**Release** (PR #16): `.goreleaser.yaml` builds `./cmd/nurtuo` for linux/windows/darwin x
amd64/arm64 with `-trimpath`, checksums, SPDX SBOMs (Syft), deb/rpm containing the systemd unit;
`make snapshot` runs `goreleaser release --snapshot --clean --skip=sign`;
`.github/workflows/release-snapshot.yml` runs it on pull requests touching the release files or
`core/runtime/**`, nightly at 04:00 UTC and on demand. Cosign signing, MSI, notarization and the
control plane binary are not part of the dry-run (`TODO(§7.1)` in both files).

## Contracts

| Contract | Stability | Notes |
|---|---|---|
| Subcommand names and exit codes (0 ok, 1 failure, 2 usage) | stable | `main.go` |
| `run --config`, `service <action> --config`, default config paths per OS | stable | `defaultConfigPath` |
| `standalone` flags and `NURTUO_DATA_DIR` | stable, except `--dev` (reserved) | `runStandalone` |
| Service name `nurtuo` and the generated service arguments | stable | upgrades depend on it |
| Data directory layout (`postgres/`, `logs/`, `secrets/`, `secrets/store`) | stable | documented in operations.md |
| `nurtuo role <name>` | experimental (stub) | draft spec `distributed-runtime-roles` |
| Control plane child environment (`DATABASE_URL`, `PORT`, `GATEWAY_*`, `NURTUO_AUTH_SECRET`, `NURTUO_BOOTSTRAP_*`) | internal | between `nurtuo` and `core/control-plane` |
| Release artifacts (archive names, deb/rpm `nurtuo`) | experimental | `.goreleaser.yaml` |

## Security

- Standalone generates all credentials locally; the CA key is discarded after issuing, so no new
  runtime certificate can be issued from it (a feature for the single-host case, a dead end for
  adding runtimes).
- `admin-token` doubles as the control plane HMAC auth secret and the bootstrap admin password:
  anyone who reads the file has full control.
- The embedded database listens on loopback with `sslmode=disable`.
- The service runs with the default service account's privileges; bridges it starts later would
  inherit them (see the bridges spec).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestDefaultConfigPath`, `TestParseServiceArgs`, `TestServiceConfigArguments`, `TestDispatchServiceErrors`, `TestProgramStartStop`, `TestProgramStopWithoutStartAndRunError` in `cmd/nurtuo/service_test.go` | service wiring | `cd core/runtime && GOWORK=off go test ./cmd/nurtuo/...` | the OS service manager is never invoked |
| `TestChildRestartsWithBackoffAndCapturesLogs`, `TestChildStopsOnCancel`, `TestWaitHealthy`, `TestShutdownOrdering`, `TestUnhealthyControlPlaneStopsDBAfterCP`, `TestDBStartFailure`, `TestDefaultDataRoot`, `TestLocalBootstrapIdempotentAndUsableForMTLS`, `TestResolveControlPlaneEntry`, `TestControlPlaneEnvWiresGatewayAndAuth` in `cmd/nurtuo/standalone_test.go` | standalone pieces | same | fakes for DB, control plane and runtime; no test runs `runRealRuntime` against the real gateway |
| `secret_test.go` | `nurtuo secret` | same | see the connections and secrets spec |
| release snapshot | goreleaser config builds | `make snapshot`; `release-snapshot` workflow | signing skipped |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | **Fixed (#56).** A user runs `nurtuo standalone` and deploys a flow: the runtime sends `Hello.runtime_id = "standalone"` but its certificate CN is `nurtuo-runtime` (`standalone_bootstrap.go`), so `core/control-plane/src/gateway.ts` rejects the session with `PERMISSION_DENIED`; nothing registers a runtime either (`touchRuntime` must find it). The runtime logs `gateway session ended, reconnecting` forever and no deployment ever applies. Found by reading the code, not by running it | critical | standalone | fixed in #56: the client certificate carries the URI SAN `spiffe://nurtuo/runtime/standalone` (`standaloneRuntimeID`, shared with `Hello.runtime_id`; a bootstrap reissues the PKI when an older client certificate lacks it), `Stack.Register` registers runtime `standalone` (environment `standalone`) over the REST API before the runtime starts (skipped when it already exists), and `TestStandaloneE2EHandshakeAndDeploy` deploys a flow end to end (placeholder deployer until gap 2) |
| 2 | Even with the identity fixed, `runRealRuntime` sets no `BundleDir`, so the runtime uses the placeholder deployer and reports `running` for flows it never starts | high | standalone | set `BundleDir` to `<data-dir>/bundles` (and `TrustedKeys` from a locally generated signing key) until `FetchArtifact` exists |
| 3 | **Fixed (#59).** Under the OS service, `RunWith` returns an error (for example an unreadable client key) inside `program.Start`'s goroutine; the error is logged but `svc.Run` keeps the process alive, so systemd/Windows see a running service and `Restart=on-failure` / `OnFailure=restart` never fire | high | service | exit the process (or call `svc.Stop` with a non-zero exit) when `run` returns an error; add a test with a failing `prg.run`; fixed in #59 by `program.exit` (`os.Exit(1)` when `run` fails on its own), see [service-exit-on-runtime-fail](../2026-11-24-service-exit-on-runtime-fail.md) |
| 4 | `nurtuo service install` on Linux creates a unit running as root (no `UserName` in `serviceConfig`), while the packaged unit runs as `nurtuo`; the same host behaves differently depending on the install path, and the root variant gives every bridge and connector root | medium | service | set `UserName` (configurable) and create the user in deb/rpm post-install scripts |
| 5 | `standalone` reacts only to `os.Interrupt`; `systemctl stop` or `docker stop` sends SIGTERM, Go's default handler exits immediately, and the embedded PostgreSQL and Node children are orphaned (the next start fails on the port or the postmaster lock) | medium | standalone | add `syscall.SIGTERM` to `signal.NotifyContext` in `runStandalone` |
| 6 | The leaf certificates expire after 2 years; there is no renewal, and the CA key was discarded, so the only fix documented is deleting `secrets/`, which also deletes `db-password` and makes the existing database unusable | medium | standalone | keep the CA key (0600) and renew leaves on start when they expire within 30 days |
| 7 | `log_level` is parsed but ignored; operators cannot raise verbosity to debug a field issue | low | run | wire it into the `slog` handler level |
| 8 | Release artifacts are unsigned (`--skip=sign` in snapshot mode; the tagged release path was never run) and there is no MSI or notarized macOS build, so Windows SmartScreen and Gatekeeper block the binary | medium | release | draft spec `release-signing-and-installers` (another area) |
| 9 | `nurtuo role` is documented in the usage text and the README promises clusters and HA, but exits 1 | medium | roles | draft spec `distributed-runtime-roles` |

## Improvement opportunities

- `nurtuo doctor`: check config, certificates (expiry, identity vs `runtime_id`), gateway
  reachability and secret references, printing the fix for each failure.
- `nurtuo standalone --print-config` to emit a `runtime.yaml` for moving from standalone to a
  separate control plane.

## References

- Open Mercato: `cli-standalone-app-support` (one command for a local install),
  `production-bootstrap-credentials` (generated bootstrap credentials and their rotation).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
