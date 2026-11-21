// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// runBatch serves c over bufconn, sends docs (IDs "0", "1", ...) and returns results by ID.
func runBatch(t testing.TB, c connector.Connector, start *connectorv1.ExecuteStart, docs []string) map[string]*connectorv1.DocumentResult {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = connector.Serve(lis, c) }()
	defer func() { _ = lis.Close() }()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	st, err := connectorv1.NewConnectorServiceClient(cc).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	send := func(m *connectorv1.ExecuteRequest) {
		if err := st.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Start{Start: start}})
	for i, d := range docs {
		send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: &connectorv1.DocumentChunk{DocumentId: strconv.Itoa(i), Data: []byte(d), Last: true}}})
	}
	send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}})
	_ = st.CloseSend()
	out := map[string]*connectorv1.DocumentResult{}
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if r := m.GetResult(); r != nil {
			out[r.DocumentId] = r
		}
	}
}
