<!-- SPDX-License-Identifier: Apache-2.0 -->
# Operations

How to install and run the `nurtuo` runtime, based on `core/runtime/cmd/nurtuo` and
`core/runtime/internal/runtime`. Nurtuo is in early development: this page documents what the code
does today. Installing an OS service, starting standalone mode against a real embedded PostgreSQL and
every Linux or macOS statement below were **not run** here; they come from the code, its unit tests
and the `kardianos/service` library the code uses.

## Commands

```
nurtuo version                         print version, commit, build date
nurtuo run [--config P]                run the runtime in the foreground (Ctrl+C / SIGTERM stops it)
nurtuo secret <set|get|list|delete>    manage the secrets that bundle connections reference
nurtuo role <name>                     not implemented yet (exit 1)
nurtuo standalone [flags]              runtime + control plane + embedded Postgres on one host
nurtuo service <action> [--config P]   install | uninstall | start | stop | status | run
```

Any other command prints the usage text and exits 2. A local build prints `nurtuo dev (none,
unknown)` for `version`; release builds inject the values through `-ldflags`.

`nurtuo run` takes only `--config` (default: the per-OS path of the service config below) and is
what `service run` executes under the OS service manager. There is no `--dev` or `--data-dir`
flag on `run`; `standalone` has `--data-dir`.

## Runtime as an OS service

`nurtuo service install` registers the service named `nurtuo` ("Nurtuo Runtime") with the OS
service manager through `github.com/kardianos/service`: Windows Service Control Manager, systemd on
Linux, launchd on macOS. The registered command line is
`nurtuo service run --config <absolute path to the config file>`. Options set by the code: restart
on failure (systemd `Restart=on-failure`; Windows recovery action restart after 5 s) and, for
launchd, `KeepAlive` and `RunAtLoad`.

| Action | Effect |
|---|---|
| `install` | loads and validates the config first and refuses to install if it is invalid, then registers the service |
| `uninstall`, `start`, `stop` | control the registered service |
| `status` | prints `running`, `stopped`, `unknown` or `not installed` |
| `run` | what the service manager invokes; also usable in a terminal (Ctrl+C stops it) |

`--config` defaults per OS:

| OS | Default config path |
|---|---|
| Windows | `%ProgramData%\Nurtuo\runtime.yaml` (`C:\ProgramData\Nurtuo\runtime.yaml` if the variable is unset) |
| macOS | `/Library/Application Support/Nurtuo/runtime.yaml` |
| Linux and others | `/etc/nurtuo/runtime.yaml` |

Installing a service needs administrator or root rights (the OS service manager decides; not
verified). Stopping waits up to 20 seconds for a graceful shutdown.

Steps, per OS (not run):

```powershell
# Windows, elevated PowerShell
New-Item -ItemType Directory -Force "$env:ProgramData\Nurtuo" | Out-Null
# create $env:ProgramData\Nurtuo\runtime.yaml (keys below) and the certificate files
.\nurtuo.exe service install
.\nurtuo.exe service start
.\nurtuo.exe service status
```

```sh
# Linux / macOS
sudo install -m 0755 nurtuo /usr/local/bin/nurtuo
# create the config at the default path (keys below) and the certificate files
sudo nurtuo service install
sudo nurtuo service start
sudo nurtuo service status
```

## Config file (`runtime.yaml`)

YAML, or JSON when the file extension is `.json`. Loaded by `LoadConfig`; relative `cert_file`,
`key_file` and `ca_file` paths are resolved against the directory of the config file.

| Key | Required | Meaning |
|---|---|---|
| `runtime_id` | yes | runtime identity; must match the mTLS client certificate identity (see [security.md](security.md)) |
| `control_plane_url` | yes | gateway address as `host:port` (gRPC) |
| `cert_file` | yes | client certificate (PEM) |
| `key_file` | yes | client private key (PEM) |
| `ca_file` | yes | CA certificate (PEM) that signed the control plane's server certificate |
| `server_name` | no | TLS server name override (SNI and verification name) |
| `log_level` | no | parsed, but **not used** by the code today: the logger does not read it |
| `bundle_dir` | no | directory of bundle files; when empty the runtime still connects but cannot run anything: it logs a warning and reports every deployment as `error` ("no bundle_dir configured"), never `running` |
| `trusted_keys` | no | list of hex Ed25519 public keys. When set, `bundle_dir` holds signed `<sha256>.bundle` archives that are digest-checked and signature-verified before use, and plain `.json` bundles are **not** accepted |
| `http_addr` | no | listen address of HTTP-trigger listeners; default `127.0.0.1:0` (random loopback port). On a non-loopback address a listener starts only if its `start` step sets `auth` (or `insecure: true`); see [connectors/http.md](connectors/http.md#http-listener-start-step) |
| `secrets` | no | `{provider: env\|dir\|encrypted-file, path, key_file}`: where `secret://name` references in bundle connections are resolved. Default `env` (`NURTUO_SECRET_<NAME>`). Manage values with `nurtuo secret`. Windows (upgrade step for service installs): an existing `path` or `key_file` must have a private DACL, or the runtime refuses to start with `icacls` fix guidance; Nurtuo does not change it ([security.md](security.md#windows-private-secrets-directory)) |
| `connection_test` | no | `strict` (default: a failing connection `Test` fails the deployment), `warn` or `off` |

```yaml
runtime_id: plant-1
control_plane_url: cp.example.com:8443
cert_file: certs/client.pem
key_file: certs/client.key
ca_file: certs/ca.pem
server_name: cp.example.com
bundle_dir: bundles
```

Behaviour of a running runtime: it dials the control plane (outbound only), sends `Hello` with its
id, version, OS, architecture and last applied desired-state revision, sends a heartbeat every
15 s, applies `DesiredState` through the supervisor, reports deployment states `applying`,
`running`, `paused`, `error`, and executes control commands. `pause_listener` and
`resume_listener` (argument `listener` = `<deployment>/<flow>`) are implemented; `replay_dlq`,
`run_now` and `test_execute` are names in the gateway contract without a handler in the runtime,
so they are answered with `ok=false`, "unknown command". On disconnect it keeps running its last applied state and reconnects with
exponential backoff from 1 s to 60 s with jitter (reset after a session longer than 30 s). The
applied revision is kept in memory only, so after a restart the runtime reports revision 0 and the
control plane resends the desired state.

With `bundle_dir` set, a deployment's `bundle_digest` (a leading `sha256:` is stripped) is read as
`<bundle_dir>/<digest>.json` (one flow document or `{"flows": [...]}`), or, when `trusted_keys` is
set, as a signed `<bundle_dir>/<digest>.bundle` that is verified first (see
[security.md](security.md)). Each flow is validated with the Go flow validator, and flows with an HTTP
or queue start step get a listener. Connections referenced by connector steps are taken from the
bundle, their `secret://` values are resolved through the `secrets` store, and each connection is
tested at deploy time according to `connection_test`.

Limits of the current runtime process, because they surprise operators: queues use the in-memory
backend (embedded JetStream is not wired into the runtime process yet), there are no `queue` or
`notify` step runners, `script` steps run in the WASM sandbox but the runtime has no way yet to
deliver script modules (a script step fails with "script is not registered"), only the native
connectors `http`, `sftp`, `postgres` and `nats` are available (bridges are not configurable from
`runtime.yaml` yet), and the HTTP listener binds `127.0.0.1:0` unless `http_addr` is set. See
[flow-reference.md](flow-reference.md).

## Standalone mode

`nurtuo standalone` runs, on one host: a locally generated certificate authority and secrets, an
embedded PostgreSQL (via `fergusstrange/embedded-postgres`, which downloads PostgreSQL binaries
into the cache directory on first use, so the first start needs network access), the control plane
as a supervised child process, and the runtime connected to it. Shutdown order is the reverse:
runtime, control plane, database. Ctrl+C (interrupt) triggers it.

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `--data-dir` | per-OS default below, or `NURTUO_DATA_DIR` | data directory |
| `--port` | 8080 | control plane HTTP port (passed as `PORT`) |
| `--control-plane-cmd` | `node` | executable that runs the control plane |
| `--control-plane-entry` | `core/control-plane/dist/main.js` | script passed to it; a relative path is searched from the executable's directory and the working directory upwards; empty means none |
| `--gateway-addr` | `127.0.0.1:8443` | gateway `host:port` the runtime dials |
| `--dev` | false | accepted and ignored ("reserved") |

The control plane child gets `DATABASE_URL` (the embedded Postgres, user `nurtuo`, database
`nurtuo`, `sslmode=disable`, on a free loopback port), `PORT`, the mTLS runtime gateway
(`GATEWAY_CERT`, `GATEWAY_KEY`, `GATEWAY_CA` from `secrets/`, `GATEWAY_PORT` from `--gateway-addr`)
and authentication (`NURTUO_AUTH_SECRET` and a bootstrap owner `admin@nurtuo.local`, both derived
from `secrets/admin-token`). It is restarted with exponential
backoff (500 ms up to 30 s); the supervisor gives up after 10 consecutive restarts that each
ran less than 30 s, and standalone then exits with an error. Standalone waits up to 60 s for the
control plane to answer `GET /`.

Limitation (#58, phase 2 tracked in #64): standalone has no bundle source yet (`bundle_dir` is not
set), so the runtime connects but cannot run deployments. They are reported as `error` with
"no bundle_dir configured" and appear as `failed` in the control plane, never `running`. Running
flows in standalone needs `FetchArtifact` (bundle artifact delivery).

The control plane serves the runtime gateway (mTLS, TLS 1.3, runtime identity taken from the client
certificate) on `--gateway-addr`. Nothing was run end to end against a real embedded PostgreSQL; the
pieces are covered by unit and cross-language interop tests.

Default data directory (`NURTUO_DATA_DIR` overrides everything):

| OS | Default |
|---|---|
| Windows | `%ProgramData%\Nurtuo`, else `%LOCALAPPDATA%\Nurtuo` |
| macOS | `~/Library/Application Support/Nurtuo` |
| Linux | `$XDG_DATA_HOME/nurtuo`, else `~/.local/share/nurtuo` |

## Data directory layout (standalone)

```
<data-dir>/
  postgres/data/       PostgreSQL cluster
  postgres/runtime/    extracted PostgreSQL binaries
  postgres/cache/      downloaded binary cache
  logs/control-plane.log   control plane output (also echoed to stderr)
  secrets/             mode 0700 (Windows: private DACL, see below)
    ca.pem             local CA (ECDSA P-256, valid 10 years)
    server.pem, server.key   control plane server certificate (2 years)
    client.pem, client.key   runtime client certificate (2 years, key mode 0600)
    db-password        random database password (mode 0600)
    admin-token        random admin token (mode 0600): control plane auth secret and password of admin@nurtuo.local
```

On Windows, `secrets\` and everything in it get a protected DACL for the running account,
`SYSTEM` and `BUILTIN\Administrators` only (other local users cannot read it or create files in
it). Each `nurtuo standalone` start verifies it and tightens a too-open DACL, printing
`standalone: tightened permissions on <path>` to stderr; an install made before this change is
repaired this way on its first start. If an object in `secrets\` is owned by another account, the
start is refused (exit 1) with the path and the owner SID: check the contents, then remove it or
take ownership. If the material may have been read by other local users before the upgrade, rotate
the connection secrets, the admin token and the database password. Details and the rules for
operator-chosen `dir` / `encrypted-file` paths (checked, never changed, with `icacls` fix guidance):
[security.md](security.md#windows-private-secrets-directory). The rest of the data root keeps the
inherited ACL (issue [#78](https://github.com/nurtuo/nurtuo/issues/78)).

Bootstrap is idempotent: existing secrets are reused. Certificates are issued for `localhost`,
`127.0.0.1` and `::1`. There is no automatic renewal: when the 2-year leaf certificates expire,
standalone mode will fail to connect until you delete `secrets/` (which also removes the database
password, so recreate the data directory as well).

## Canvas test runs (EXPERIMENTAL)

The control plane can run a canvas flow once through `nurtuo flow run` (see
[the spec](specs/2026-11-23-canvas-run.md)). Environment variables of the control plane:

| Variable | Meaning |
| --- | --- |
| `NURTUO_FLOW_RUN_BIN` | Absolute path of the `nurtuo` binary used for test runs. Unset or not executable: `POST /v1/workspaces/{ws}/test-runs` answers 503 `test_run_unavailable`. `nurtuo standalone` sets it for its control plane. |
| `NURTUO_TEST_RUN_ALLOW_PRIVATE` | `1` lets test runs and connections reach loopback, private and link-local addresses. Off by default (SSRF protection); turn it on only for local development and tests. |

Workspace secrets set from the canvas live in the control plane's memory and are lost on restart (run
`secret_missing` until they are entered again). Request limits depend on the operation. Sign-in accepts up to 64 KiB and test run requests
accept up to 1 MiB. Connection saves and host-key scans have their own limits. Requests that
exceed the applicable limit return 413 `payload_too_large`. The default for revision uploads
continues to have no byte ceiling.

## Logs

- Standalone: control plane output goes to `logs/control-plane.log` and to stderr; the runtime logs
  with Go `slog` text format to stderr.
- Service: when run by the service manager the runtime's `slog` lines are forwarded to the OS logger
  (Windows Event Log, journald or syslog on Linux/macOS, through `kardianos/service`) and mapped by
  `level=ERROR`, `level=WARN` and everything else to info. In a terminal they go to stderr.
  On Linux read them with `journalctl -u nurtuo`; on Windows in Event Viewer, Windows Logs,
  Application (not verified).
- Log volume is not configurable today (`log_level` is ignored).

## Upgrade

1. Download or build the new `nurtuo` binary.
2. `nurtuo service stop`, replace the binary at the path the service was installed with, then
   `nurtuo service start`. The service definition stores the binary path and config path; if
   either changes, `uninstall` and `install` again.
3. The runtime needs no migration of local state (the applied revision is in memory and the
   control plane resends desired state). Control plane database migrations are SQL files in
   `core/control-plane/migrations`, applied by the control plane at start when `DATABASE_URL` is
   set.

Compatibility policy (from `ARCHITECTURE.md`): runtime and control plane are meant to support N-1
minor versions against each other; this is not yet enforced or tested.

## Backup

- Runtime-only install: back up `runtime.yaml`, the certificate and key files and `bundle_dir`.
  There is no other persistent runtime state yet (queues are in memory in the runtime process); if you use the `dir` or `encrypted-file` secrets provider, also back up its `path` (and `key_file`).
- Standalone: stop `nurtuo standalone` and copy the whole data directory, including `secrets/`
  (without it the CA and database password are lost). A running-database backup with `pg_dump`
  against the embedded instance was not tried.
- Treat `secrets/` and the runtime key file as secrets; see [security.md](security.md).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `refusing to install with invalid config: ...` | `service install` validates the config; fix the message (missing file, parse error, `config: missing required fields: ...`) |
| `config: missing required fields: runtime_id, ...` | the five required keys: `runtime_id`, `control_plane_url`, `cert_file`, `key_file`, `ca_file` |
| `mtls: load client cert: ...` / `read CA: ...` / `CA file contains no certificates` | wrong certificate, key or CA path or content; relative paths are relative to the config file |
| repeated `gateway session ended, reconnecting` | control plane unreachable, TLS verification failed (check `server_name` and the CA), or the gateway is not served at `control_plane_url`; the runtime keeps its last state and retries |
| TLS 1.3 handshake failure | the client requires TLS 1.3 minimum; the server must support it |
| `standalone: no data directory; use --data-dir or NURTUO_DATA_DIR` | the OS default could not be determined |
| standalone exits with `supervisor: control-plane exceeded 10 restarts` | the child keeps crashing: read `logs/control-plane.log`; check that `node` is on PATH and `core/control-plane/dist/main.js` exists (`pnpm --filter @nurtuo/control-plane build`) |
| `start embedded postgres: ...` | first start needs network to fetch binaries into `postgres/cache`; check disk space and that the data directory is writable; a leftover postmaster process can hold the port |
| a deployment is `error` with `no_runner` documents | the step kind has no runner in the runtime process (queue, notify); see [flow-reference.md](flow-reference.md) |
| a connection fails the deployment at deploy time | `connection_test: strict` (default) runs the connector's `Test`; fix the connection or secret, or set `warn` / `off` |

## Container and package notes

`core/deploy/helm`, `launchd` and `windows` contain only placeholder READMEs ("planned"). The
systemd directory has a unit template for packaging (shipped in the deb/rpm by `.goreleaser.yaml`)
that runs `nurtuo run --config /etc/nurtuo/runtime.yaml` as user `nurtuo`; the package does not
create that user or the config file, so create them before starting the unit (not run).
