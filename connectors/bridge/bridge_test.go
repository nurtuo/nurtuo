// SPDX-License-Identifier: Apache-2.0

package bridge_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/connectors/bridge"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func networks() []string {
	if runtime.GOOS == "windows" {
		return []string{"tcp"}
	}
	return []string{"tcp", "unix"}
}

var fakeCases = []contracttest.ExecuteCase{{
	Name:  "per-document status",
	Start: &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE},
	Documents: []contracttest.Document{
		{ID: "d1", Data: []byte("good")},
		{ID: "d2", Data: []byte("bad payload")},
		{ID: "d3", Data: bytes.Repeat([]byte("x"), 300_000)},
	},
	ChunkSize: 50_000,
	Want: map[string]connectorv1.DocumentStatus{
		"d1": connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
		"d2": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
		"d3": connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
	},
}}

func TestFakeBridgeConformsViaDial(t *testing.T) {
	for _, network := range networks() {
		t.Run(network, func(t *testing.T) {
			b := start(t, fakeConfig(t, "ok", network))
			if got := b.Implementation(); got != "fake/1.0" {
				t.Errorf("Implementation() = %q", got)
			}
			contracttest.Run(t, contracttest.Options{
				// The suite closes what Dial returns, so hand it its own connection.
				Dial: func(t testing.TB) *grpc.ClientConn {
					cc, err := b.NewConn()
					if err != nil {
						t.Fatal(err)
					}
					return cc
				},
				Execute: fakeCases,
			})
		})
	}
}

func TestConnectorAdapterConforms(t *testing.T) {
	b := start(t, fakeConfig(t, "ok", ""))
	contracttest.Run(t, contracttest.Options{
		New:     func(testing.TB) connector.Connector { return b.Connector() },
		Execute: fakeCases,
	})
}

func TestEnvironmentIsNotInherited(t *testing.T) {
	t.Setenv("NURTUO_TEST_SECRET", "hunter2")
	envFile := filepath.Join(t.TempDir(), "env")
	start(t, fakeConfig(t, "ok", "", "NURTUO_FAKE_ENVFILE="+envFile, "EXPLICIT=yes"))
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "|yes" { // secret absent, explicit Config.Env present
		t.Fatalf("child env = %q, want the runtime secret to be absent and EXPLICIT=yes present", got)
	}
}

func TestTokenIsRequiredByTheBridge(t *testing.T) {
	b := start(t, fakeConfig(t, "ok", ""))
	network, addr := b.Address()
	cc, err := grpc.NewClient("passthrough:///x",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = connectorv1.NewConnectorServiceClient(cc).Describe(ctx, &connectorv1.DescribeRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Describe without token: %v, want Unauthenticated", err)
	}
	// ...while the launcher's own connection carries it.
	if _, err := b.Client().Describe(ctx, &connectorv1.DescribeRequest{}); err != nil {
		t.Fatalf("Describe through the bridge client: %v", err)
	}
}

func TestRestartAfterCrash(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	b := start(t, fakeConfig(t, "crash-once", "", "NURTUO_FAKE_MARKER="+marker))
	eventually(t, 15*time.Second, "restart", func() bool { return b.Restarts() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The same ClientConn works against the new process (RPC waits for it if needed).
	d, err := b.Connector().Describe(ctx)
	if err != nil || d.GetId() != "fake-bridge" {
		t.Fatalf("Describe after restart: %v, %v", d, err)
	}
	if b.Err() != nil {
		t.Fatalf("Err() = %v", b.Err())
	}
}

func TestRestartBudgetExhausted(t *testing.T) {
	cfg := fakeConfig(t, "always-crash", "")
	cfg.MaxRestarts = 2
	b := start(t, cfg)
	select {
	case <-b.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("bridge did not fail after exhausting its restart budget")
	}
	if err := b.Err(); err == nil || !strings.Contains(err.Error(), "restart budget") {
		t.Fatalf("Err() = %v", err)
	}
	if got := b.Restarts(); got != 2 {
		t.Errorf("Restarts() = %d, want 2", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.Connector().Describe(ctx); err == nil {
		t.Fatal("RPC on a failed bridge succeeded")
	}
}

func TestNoRestartWhenDisabled(t *testing.T) {
	cfg := fakeConfig(t, "always-crash", "")
	cfg.MaxRestarts = -1
	b := start(t, cfg)
	select {
	case <-b.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not fail")
	}
	if b.Restarts() != 0 {
		t.Errorf("Restarts() = %d", b.Restarts())
	}
}

func TestCrashMidBatchIsNeverSuccess(t *testing.T) {
	cfg := fakeConfig(t, "ok", "")
	cfg.MaxRestarts = -1
	b := start(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := b.Client().Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []*connectorv1.ExecuteRequest{
		{Msg: &connectorv1.ExecuteRequest_Start{Start: &connectorv1.ExecuteStart{ExecutionId: "e"}}},
		{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: &connectorv1.DocumentChunk{DocumentId: "d1", Data: []byte("please crash"), Last: true}}},
		{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}},
	} {
		if err := stream.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	_ = stream.CloseSend()
	var results int
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.OK {
				t.Fatalf("stream ended with %v, want a non-OK status", err)
			}
			break
		}
		if resp.GetResult() != nil {
			results++
		}
	}
	if results != 0 {
		t.Errorf("got %d results from a crashed bridge", results)
	}
}

func TestStartFailures(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		timeout time.Duration
		want    string
	}{
		{"exits before handshake", "exit-early", 0, "exited before the handshake"},
		{"no handshake", "silent", 400 * time.Millisecond, "no handshake"},
		{"garbage handshake", "garbage", 0, "not a handshake line"},
		{"unoffered contract", "badcontract", 0, "not offered"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fakeConfig(t, tc.mode, "")
			if tc.timeout > 0 {
				cfg.StartTimeout = tc.timeout
			}
			b, err := bridge.Start(context.Background(), cfg)
			if err == nil {
				_ = b.Close()
				t.Fatal("Start succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := bridge.Start(context.Background(), bridge.Config{}); err == nil {
		t.Error("Start without a command succeeded")
	}
	if _, err := bridge.Start(context.Background(), bridge.Config{Command: filepath.Join(t.TempDir(), "does-not-exist")}); err == nil {
		t.Error("Start with a missing binary succeeded")
	}
}

func TestStartHonoursContext(t *testing.T) {
	cfg := fakeConfig(t, "silent", "")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if _, err := bridge.Start(ctx, cfg); err == nil {
		t.Fatal("Start succeeded")
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatal("Start ignored its context")
	}
}

func TestCloseIsGracefulAndRemovesTheSocket(t *testing.T) {
	for _, network := range networks() {
		t.Run(network, func(t *testing.T) {
			exitFile := filepath.Join(t.TempDir(), "exit")
			b := start(t, fakeConfig(t, "ok", network, "NURTUO_FAKE_EXITFILE="+exitFile))
			nw, addr := b.Address()
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(exitFile); string(got) != "graceful" {
				t.Errorf("child did not exit gracefully on stdin EOF (exit file: %q)", got)
			}
			if nw == "unix" {
				if _, err := os.Stat(addr); err == nil {
					t.Errorf("socket %s still exists after Close", addr)
				}
			}
			if c, err := net.DialTimeout(nw, addr, time.Second); err == nil {
				_ = c.Close()
				t.Error("child still accepts connections after Close")
			}
			if err := b.Close(); err != nil { // idempotent
				t.Fatal(err)
			}
			select {
			case <-b.Done():
			default:
				t.Error("Done not closed after Close")
			}
		})
	}
}

func TestCloseKillsAnUnresponsiveChild(t *testing.T) {
	cfg := fakeConfig(t, "ignore-stdin", "")
	cfg.StopTimeout = 300 * time.Millisecond
	b := start(t, cfg)
	nw, addr := b.Address()
	begin := time.Now()
	_ = b.Close()
	if d := time.Since(begin); d > 10*time.Second {
		t.Fatalf("Close took %s", d)
	}
	if c, err := net.DialTimeout(nw, addr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("child survived Close")
	}
}

func TestNoisyStdoutDoesNotBlockTheChild(t *testing.T) {
	b := start(t, fakeConfig(t, "noisy", ""))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.Connector().Describe(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestParseHandshake(t *testing.T) {
	ok := []struct {
		line string
		want bridge.Handshake
	}{
		{"NURTUO-BRIDGE|1|tcp|127.0.0.1:5000|nurtuo.connector.v1|python/0.1.0\n",
			bridge.Handshake{Version: 1, Network: "tcp", Address: "127.0.0.1:5000", Contract: "nurtuo.connector.v1", Implementation: "python/0.1.0"}},
		{"NURTUO-BRIDGE|1|unix|/tmp/a b/x.sock|nurtuo.connector.v1\r\n",
			bridge.Handshake{Version: 1, Network: "unix", Address: "/tmp/a b/x.sock", Contract: "nurtuo.connector.v1"}},
		{"NURTUO-BRIDGE|1|tcp|[::1]:8080|nurtuo.connector.v1|jvm/1.2|with|pipes",
			bridge.Handshake{Version: 1, Network: "tcp", Address: "[::1]:8080", Contract: "nurtuo.connector.v1", Implementation: "jvm/1.2|with|pipes"}},
	}
	for _, tc := range ok {
		got, err := bridge.ParseHandshake(tc.line)
		if err != nil || got != tc.want {
			t.Errorf("ParseHandshake(%q) = %+v, %v; want %+v", tc.line, got, err, tc.want)
		}
	}
	bad := []string{
		"",
		"hello",
		"NURTUO-BRIDGE|1|tcp|127.0.0.1:5000",
		"NURTUO-BRIDGE|2|tcp|127.0.0.1:5000|nurtuo.connector.v1",
		"NURTUO-BRIDGE|x|tcp|127.0.0.1:5000|nurtuo.connector.v1",
		"NURTUO-BRIDGE|1|tcp|10.0.0.5:5000|nurtuo.connector.v1",
		"NURTUO-BRIDGE|1|tcp|example.com:5000|nurtuo.connector.v1",
		"NURTUO-BRIDGE|1|tcp|127.0.0.1:0|nurtuo.connector.v1",
		"NURTUO-BRIDGE|1|tcp|127.0.0.1|nurtuo.connector.v1",
		"NURTUO-BRIDGE|1|unix||nurtuo.connector.v1",
		"NURTUO-BRIDGE|1|pipe|x|nurtuo.connector.v1",
	}
	for _, line := range bad {
		if _, err := bridge.ParseHandshake(line); err == nil {
			t.Errorf("ParseHandshake(%q) succeeded", line)
		}
	}
}
