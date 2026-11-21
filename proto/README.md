# proto/ (Apache-2.0)

gRPC/protobuf contracts, managed with [buf](https://buf.build).

- `nurtuo/connector/v1/connector.proto`: connector contract (Describe / Browse / Execute / Test / Listen)
  for native, bridged and WASM connectors.
- `nurtuo/runtime/v1/gateway.proto`: outbound-only runtime <-> control plane stream (desired state, status, commands).

`make gen` regenerates Go (`sdk/go/gen`) and TypeScript (`sdk/ts/proto/src/gen`). Generated code is committed.
CI runs `buf lint`, `buf breaking` against `main`, and fails if generated code is stale.
