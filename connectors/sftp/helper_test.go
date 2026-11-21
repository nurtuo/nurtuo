// SPDX-License-Identifier: Apache-2.0

package sftp_test

import (
	"context"
	"net"
	"testing"

	"github.com/nurtuo/nurtuo/connectors/sftp"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// dialConnector serves the SFTP connector through the real gRPC server over an in-memory listener.
func dialConnector(t *testing.T) connectorv1.ConnectorServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = connector.Serve(lis, sftp.New()) }()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close(); _ = lis.Close() })
	return connectorv1.NewConnectorServiceClient(cc)
}
