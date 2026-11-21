# Licensing

This repository contains code under two licenses. The license of a file is determined by
the nearest `LICENSE` file in its directory or a parent directory.

| Path | License | SPDX |
|---|---|---|
| `/` (root files), `core/**` | GNU Affero General Public License v3.0 | `AGPL-3.0-only` |
| `proto/**` | Apache License 2.0 | `Apache-2.0` |
| `sdk/**` | Apache License 2.0 | `Apache-2.0` |
| `connectors/**` | Apache License 2.0 | `Apache-2.0` |
| `bridges/**` | Apache License 2.0 | `Apache-2.0` |
| `cli/**` | Apache License 2.0 | `Apache-2.0` |
| `examples/**`, `docs/**` | Apache License 2.0 | `Apache-2.0` |

## Rules enforced in CI

1. **Dependency direction.** AGPL code may depend on Apache-2.0 code. Apache-2.0 code must
   never depend on, import or vendor anything from `core/` (`scripts/check-boundaries.sh`).
2. **Third-party allow-lists.** AGPL artifacts: `.github/licenses/agpl-allow.txt`.
   Apache-2.0 artifacts: `.github/licenses/apache-allow.txt` (stricter: no GPL/AGPL).
3. **SPDX headers.** Every source file starts with an `SPDX-License-Identifier` matching its directory.
4. **Customer-supplied components** are never bundled: SAP JCo, IBM MQ client, Oracle/Db2/Teradata
   JDBC drivers, Saxon PE/EE. Bridges download or load them only after the customer accepts their license.

## Contributions

All contributions require signing the Nurtuo CLA (see CONTRIBUTING.md). The CLA lets the
project relicense contributions, for example to move code between the AGPL and Apache-2.0 parts.

## FAQ

- *Can I run Nurtuo in production for free?* Yes, all features, unmodified or modified, internally.
- *Do my flows become AGPL?* No. Flows, mappings and configuration are your data, not derivative works.
- *Can I write a closed-source connector?* Yes, on the Apache-2.0 SDK. Run it out of process over
  the gRPC connector contract (as a bridge or external connector), not compiled into the runtime binary.
- *Can I host a modified Nurtuo as a service?* Yes, if you publish your modifications under AGPL-3.0.
