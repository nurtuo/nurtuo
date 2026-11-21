# proto/ (Apache-2.0)

gRPC/protobuf contracts, managed with [buf](https://buf.build).

- `nurtuo/connector/v1/connector.proto`: connector contract (Describe / Browse / Execute / Test / Listen)
  for native, bridged and WASM connectors.
- `nurtuo/runtime/v1/gateway.proto`: outbound-only runtime <-> control plane stream (desired state, status, commands).

`make gen` regenerates Go (`sdk/go/gen`) and TypeScript (`sdk/ts/proto/src/gen`). Generated code is committed.
CI runs `buf lint`, `buf breaking` against `main`, and fails if generated code is stale.

Compatibility: within `v1` only additive changes are allowed (`buf breaking`, FILE rules). The
connector contract is exercised by the suite in `sdk/go/connector/contracttest`.
`buf generate` needs `protoc-gen-go`, `protoc-gen-go-grpc` on PATH and `pnpm install` for `protoc-gen-es`
(`mise install` pins them).
