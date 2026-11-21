module github.com/nurtuo/nurtuo/examples/connector-minimal

go 1.27

require github.com/nurtuo/nurtuo/sdk/go v0.0.0

require (
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/grpc v1.85.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/nurtuo/nurtuo/sdk/go => ../../sdk/go
