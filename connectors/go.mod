module github.com/nurtuo/nurtuo/connectors

go 1.27

require (
	github.com/jackc/pgx/v5 v5.13.0
	github.com/nats-io/nats-server/v2 v2.15.3
	github.com/nats-io/nats.go v1.56.0
	github.com/nats-io/nkeys v0.4.17
	github.com/nurtuo/nurtuo/sdk/go v0.0.0
	github.com/pkg/sftp v1.13.12
	golang.org/x/crypto v0.59.0
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.9.0-default-no-op // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/klauspost/compress v1.20.4 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/jwt/v2 v2.8.3 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
)

replace github.com/nurtuo/nurtuo/sdk/go => ../sdk/go
