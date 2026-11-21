# Nurtuo Go SDK (Apache-2.0)

- `gen/` generated protobuf/gRPC code from `proto/` (`make gen`; committed, do not edit).
- `connector/` connector SDK: implement `Connector`, register natively or `Serve` out of process.

Module: `github.com/nurtuo/nurtuo/sdk/go`, tagged as `sdk/go/vX.Y.Z`.
- `connector/contracttest/` reusable conformance suite for the connector contract. Run it from a
  connector's own tests with `contracttest.Run(t, contracttest.Options{...})`; see the package doc and
  `contracttest_test.go` (a trivial fake connector) for an example.
