// SPDX-License-Identifier: Apache-2.0

package bridge_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/connectors/bridge"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The fake bridge is this test binary re-executed with NURTUO_FAKE_BRIDGE=1 (the classic
// os/exec helper-process pattern). NURTUO_FAKE_MODE selects its behaviour.
func TestMain(m *testing.M) {
	if os.Getenv("NURTUO_FAKE_BRIDGE") == "1" {
		runFakeBridge()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fakeServer struct {
	connectorv1.UnimplementedConnectorServiceServer
}

func (fakeServer) Describe(context.Context, *connectorv1.DescribeRequest) (*connectorv1.DescribeResponse, error) {
	return &connectorv1.DescribeResponse{Descriptor_: &connectorv1.Descriptor{
		Id:          "fake-bridge",
		Version:     "0.1.0",
		Host:        connectorv1.Host_HOST_BRIDGE_PYTHON,
		DisplayName: "Fake bridge",
		Operations:  []*connectorv1.OperationDescriptor{{Type: connectorv1.OperationType_OPERATION_TYPE_CREATE}},
	}}, nil
}

func (fakeServer) Test(context.Context, *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return &connectorv1.TestResponse{Ok: true, Diagnostic: "fake reachable"}, nil
}

// Execute: "crash" in a payload kills the process mid-batch, "bad" is an application error.
func (fakeServer) Execute(s connectorv1.ConnectorService_ExecuteServer) error {
	var order []string
	data := map[string][]byte{}
	for {
		req, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if ch := req.GetChunk(); ch != nil {
			if _, ok := data[ch.GetDocumentId()]; !ok {
				order = append(order, ch.GetDocumentId())
			}
			data[ch.GetDocumentId()] = append(data[ch.GetDocumentId()], ch.GetData()...)
		}
	}
	for _, id := range order {
		if bytes.Contains(data[id], []byte("crash")) {
			os.Exit(1)
		}
		st := connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
		if bytes.Contains(data[id], []byte("bad")) {
			st = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
		}
		if err := s.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{
			Result: &connectorv1.DocumentResult{DocumentId: id, Status: st},
		}}); err != nil {
			return err
		}
	}
	return nil
}

func runFakeBridge() {
	mode := os.Getenv("NURTUO_FAKE_MODE")
	switch mode {
	case "exit-early":
		fmt.Fprintln(os.Stderr, "fake: refusing to start")
		os.Exit(3)
	case "silent":
		time.Sleep(time.Minute)
		return
	case "garbage":
		fmt.Println("this is not a handshake")
		time.Sleep(time.Minute)
		return
	}
	if marker := os.Getenv("NURTUO_FAKE_MARKER"); mode == "crash-once" && marker != "" {
		if _, err := os.Stat(marker); err != nil {
			_ = os.WriteFile(marker, []byte("crashed"), 0o600)
			go func() { time.Sleep(300 * time.Millisecond); os.Exit(1) }()
		}
	}
	if f := os.Getenv("NURTUO_FAKE_ENVFILE"); f != "" {
		_ = os.WriteFile(f, []byte(os.Getenv("NURTUO_TEST_SECRET")+"|"+os.Getenv("EXPLICIT")), 0o600)
	}
	if mode == "always-crash" {
		go func() { time.Sleep(100 * time.Millisecond); os.Exit(1) }()
	}

	network, addr, _ := strings.Cut(os.Getenv(bridge.EnvListen), ":")
	if network == "tcp" {
		addr = "127.0.0.1:0"
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake: listen:", err)
		os.Exit(2)
	}
	token := os.Getenv(bridge.EnvToken)
	auth := func(ctx context.Context) error {
		md, _ := metadata.FromIncomingContext(ctx)
		if v := md.Get(bridge.TokenMetadataKey); len(v) != 1 || v[0] != token {
			return status.Error(codes.Unauthenticated, "bad token")
		}
		return nil
	}
	g := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if err := auth(ctx); err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := auth(ss.Context()); err != nil {
				return err
			}
			return h(srv, ss)
		}),
	)
	connectorv1.RegisterConnectorServiceServer(g, fakeServer{})

	contract := bridge.ContractV1
	if mode == "badcontract" {
		contract = "nurtuo.connector.v2"
	}
	fmt.Printf("%s|%d|%s|%s|%s|fake/1.0\n", bridge.HandshakePrefix, bridge.HandshakeVersion, network, l.Addr().String(), contract)
	if mode == "noisy" {
		for i := 0; i < 1000; i++ {
			fmt.Println("noise", i)
		}
	}

	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin) // EOF: the runtime closed our stdin
		if mode == "ignore-stdin" {
			return
		}
		if f := os.Getenv("NURTUO_FAKE_EXITFILE"); f != "" {
			_ = os.WriteFile(f, []byte("graceful"), 0o600)
		}
		g.GracefulStop()
	}()
	_ = g.Serve(l)
}

type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log("bridge stderr: " + strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// fakeConfig configures the fake bridge. Network "" means auto.
func fakeConfig(t testing.TB, mode, network string, extraEnv ...string) bridge.Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return bridge.Config{
		Command:        exe,
		Network:        network,
		Env:            append([]string{"NURTUO_FAKE_BRIDGE=1", "NURTUO_FAKE_MODE=" + mode}, extraEnv...),
		StartTimeout:   10 * time.Second,
		StopTimeout:    3 * time.Second,
		RestartBackoff: 20 * time.Millisecond,
		Stderr:         testWriter{t},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func start(t testing.TB, cfg bridge.Config) *bridge.Bridge {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b, err := bridge.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func eventually(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
