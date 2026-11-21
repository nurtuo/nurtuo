# sdk/ (Apache-2.0)

| Dir | What |
|---|---|
| `go/` | Go module: generated gRPC code + connector SDK |
| `ts/flow-schema/` | Canonical flow format: JSON Schema + TS types + CommandBus command types |
| `ts/flow-validate/` | Validation library used by canvas, control plane, CLI and MCP |
| `ts/proto/` | Generated protobuf-es / Connect types |
| `ts/client/` | Public API client |

Nothing here may import from `core/`.
