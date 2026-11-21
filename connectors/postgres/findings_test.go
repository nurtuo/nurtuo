// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func cfgOf(t testing.TB, m map[string]any) *connectorv1.ConnectionConfig {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return &connectorv1.ConnectionConfig{Settings: s}
}

func hostCfg(t testing.TB, i int, password string) *connectorv1.ConnectionConfig {
	return cfgOf(t, map[string]any{
		"host": "h" + strconv.Itoa(i) + ".invalid", "database": "d", "user": "u", "password": password, "sslMode": "disable",
	})
}

func TestPoolKeyHasNoPlaintextPassword(t *testing.T) {
	cfg, err := ParseConfig(hostCfg(t, 1, "hunter2-PLAINTEXT"))
	if err != nil {
		t.Fatal(err)
	}
	if k := cfg.key(); strings.Contains(k, "hunter2") {
		t.Errorf("pool key contains the password: %q", k)
	}
}

func TestPoolsAreBoundedAndEvicted(t *testing.T) {
	c := New()
	defer c.Close()
	for i := 0; i < maxPools+5; i++ {
		_, _, release, err := c.pool(context.Background(), hostCfg(t, i, "pw"))
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	c.mu.Lock()
	n := len(c.pools)
	c.mu.Unlock()
	if n > maxPools {
		t.Errorf("%d pools cached, max %d", n, maxPools)
	}
}

func TestTestLeavesNoPool(t *testing.T) {
	c := New()
	defer c.Close()
	cfg := cfgOf(t, map[string]any{"host": "127.0.0.1", "port": float64(1), "database": "d", "user": "u", "sslMode": "disable", "connectTimeoutSeconds": float64(1)})
	resp, _ := c.Test(context.Background(), &connectorv1.TestRequest{Connection: cfg})
	if resp.GetOk() {
		t.Fatal("unexpected ok")
	}
	c.mu.Lock()
	n := len(c.pools)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("Test left %d cached pool(s)", n)
	}
}

type memStream struct {
	connectorv1.ConnectorService_ExecuteServer
	in []*connectorv1.ExecuteRequest
}

func (m *memStream) Context() context.Context { return context.Background() }
func (m *memStream) Recv() (*connectorv1.ExecuteRequest, error) {
	if len(m.in) == 0 {
		return nil, io.EOF
	}
	r := m.in[0]
	m.in = m.in[1:]
	return r, nil
}
func (m *memStream) Send(*connectorv1.ExecuteResponse) error { return nil }

func executeCode(t *testing.T, start *connectorv1.ExecuteStart) codes.Code {
	t.Helper()
	c := New()
	defer c.Close()
	err := c.Execute(&memStream{in: []*connectorv1.ExecuteRequest{{Msg: &connectorv1.ExecuteRequest_Start{Start: start}}}})
	return status.Code(err)
}

func TestInvalidConfigIsInvalidArgument(t *testing.T) {
	got := executeCode(t, &connectorv1.ExecuteStart{
		OperationType: connectorv1.OperationType_OPERATION_TYPE_QUERY,
		Connection:    cfgOf(t, map[string]any{"host": "", "database": "d", "user": "u"}),
	})
	if got != codes.InvalidArgument {
		t.Errorf("code %v, want InvalidArgument", got)
	}
}

func TestInvalidOperationTypeRejected(t *testing.T) {
	for _, st := range []*connectorv1.ExecuteStart{
		{OperationType: connectorv1.OperationType_OPERATION_TYPE_UNSPECIFIED},
		{OperationType: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: "NOPE"},
	} {
		st.Connection = hostCfg(t, 1, "pw")
		if got := executeCode(t, st); got != codes.InvalidArgument {
			t.Errorf("%v/%q: code %v, want InvalidArgument", st.OperationType, st.CustomType, got)
		}
	}
}
