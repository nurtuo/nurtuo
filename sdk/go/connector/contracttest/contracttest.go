// SPDX-License-Identifier: Apache-2.0

// Package contracttest is the reusable conformance suite for the connector contract
// (nurtuo.connector.v1). Every native connector, and any third-party connector written in Go,
// runs it from its own tests:
//
//	func TestContract(t *testing.T) {
//		contracttest.Run(t, contracttest.Options{
//			New: func(t testing.TB) connector.Connector { return &myConnector{} },
//			Connection: &connectorv1.ConnectionConfig{Settings: settings},
//			Execute: []contracttest.ExecuteCase{ ... },
//		})
//	}
//
// The suite serves the connector through the real gRPC server (connector.Serve) over an
// in-memory listener and drives it through the generated client, so it checks the wire contract
// and not only the Go interface. Bridges (JVM, Python, .NET) are covered by pointing Options.Dial
// at the bridge's socket instead of providing Options.New.
//
// Status: skeleton. Implemented: descriptor sanity, optional golden descriptor, test-connection
// redaction, per-document status semantics of Execute, bounded-memory streaming of a large
// payload (opt-in through Options.LargePayload). Planned (reported as skipped): crash behaviour,
// Listen acks.
package contracttest

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// DefaultChunkSize is the chunk size used to stream documents when ExecuteCase.ChunkSize is 0.
const DefaultChunkSize = 64 * 1024

// Document is one input document of an Execute case.
type Document struct {
	ID         string
	Properties map[string]string
	Data       []byte
}

// ExecuteCase is one Execute batch and the per-document statuses the connector must return.
type ExecuteCase struct {
	Name string
	// Start is the first message of the stream; execution_id and step_id are filled in if empty.
	Start     *connectorv1.ExecuteStart
	Documents []Document
	// Want maps document ID to the required status. Documents not listed only need a
	// non-UNSPECIFIED status.
	Want      map[string]connectorv1.DocumentStatus
	ChunkSize int
}

// Options configures Run. Exactly one of New or Dial must be set.
type Options struct {
	// New returns a fresh connector for each subtest (in-process connectors).
	New func(t testing.TB) connector.Connector
	// Dial returns a client connection to an already running connector (bridges).
	Dial func(t testing.TB) *grpc.ClientConn
	// Connection is passed to Browse, Test and Execute.
	Connection *connectorv1.ConnectionConfig
	// Golden, if set, must equal the descriptor returned by Describe.
	Golden *connectorv1.ConnectorDescriptor
	// Secrets are values that must never appear in the Test diagnostic.
	Secrets []string
	// Execute cases; if empty the Execute subtests are skipped.
	Execute []ExecuteCase
	// LargePayload enables the ExecuteLargePayload subtest for connectors that stream a document
	// incrementally; if nil the subtest is skipped.
	LargePayload *LargePayloadCase
	// Timeout per RPC; defaults to 10s.
	Timeout time.Duration
}

var semver = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// Run executes the conformance suite as subtests of t.
func Run(t *testing.T, o Options) {
	t.Helper()
	if (o.New == nil) == (o.Dial == nil) {
		t.Fatal("contracttest: set exactly one of Options.New and Options.Dial")
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}

	t.Run("Describe", func(t *testing.T) { describe(t, &o) })
	t.Run("Test", func(t *testing.T) { testConnection(t, &o) })
	t.Run("Execute", func(t *testing.T) {
		if len(o.Execute) == 0 {
			t.Skip("no Execute cases provided")
		}
		for _, c := range o.Execute {
			t.Run(c.Name, func(t *testing.T) { execute(t, &o, c) })
		}
	})
	t.Run("ExecuteLargePayload", func(t *testing.T) { executeLargePayload(t, &o) })
	t.Run("CrashBehaviour", func(t *testing.T) {
		t.Skip("TODO: connector dying mid-batch must surface as a non-OK status, never as SUCCESS")
	})
	t.Run("Listen", func(t *testing.T) {
		t.Skip("TODO: ListenStart, push, Ack semantics (needs a connector that declares OPERATION_TYPE_LISTEN)")
	})
}

// client returns a client for a fresh connector and registers cleanup.
func client(t testing.TB, o *Options) connectorv1.ConnectorServiceClient {
	t.Helper()
	if o.Dial != nil {
		cc := o.Dial(t)
		t.Cleanup(func() { _ = cc.Close() })
		return connectorv1.NewConnectorServiceClient(cc)
	}
	lis := bufconn.Listen(1 << 20)
	served := make(chan error, 1)
	go func() { served <- connector.Serve(lis, o.New(t)) }()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		_ = lis.Close()
		if err := <-served; err != nil {
			t.Logf("serve: %v", err)
		}
	})
	return connectorv1.NewConnectorServiceClient(cc)
}

func ctx(t testing.TB, o *Options) context.Context {
	c, cancel := context.WithTimeout(context.Background(), o.Timeout)
	t.Cleanup(cancel)
	return c
}

func describe(t *testing.T, o *Options) {
	resp, err := client(t, o).Describe(ctx(t, o), &connectorv1.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	d := resp.GetDescriptor_()
	if d == nil {
		t.Fatal("Describe returned no descriptor")
	}
	if d.GetId() == "" {
		t.Error("descriptor.id is empty")
	}
	if !semver.MatchString(d.GetVersion()) {
		t.Errorf("descriptor.version %q is not SemVer", d.GetVersion())
	}
	if d.GetHost() == connectorv1.Host_HOST_UNSPECIFIED {
		t.Error("descriptor.host is unspecified")
	}
	if len(d.GetOperations()) == 0 {
		t.Error("descriptor declares no operations")
	}
	for i, op := range d.GetOperations() {
		if op.GetType() == connectorv1.OperationType_OPERATION_TYPE_UNSPECIFIED {
			t.Errorf("operations[%d].type is unspecified", i)
		}
		if op.GetType() != connectorv1.OperationType_OPERATION_TYPE_EXECUTE && op.GetCustomType() != "" {
			t.Errorf("operations[%d]: custom_type is only valid for EXECUTE", i)
		}
	}
	if o.Golden != nil && !proto.Equal(o.Golden, d) {
		t.Errorf("descriptor differs from golden:\n got: %v\nwant: %v", d, o.Golden)
	}
}

func testConnection(t *testing.T, o *Options) {
	resp, err := client(t, o).Test(ctx(t, o), &connectorv1.TestRequest{Connection: o.Connection})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	for _, s := range o.Secrets {
		if s != "" && strings.Contains(resp.GetDiagnostic(), s) {
			t.Errorf("Test diagnostic leaks a secret value")
		}
	}
}

func execute(t *testing.T, o *Options, c ExecuteCase) {
	cl := client(t, o)
	stream, err := cl.Execute(ctx(t, o))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	start := &connectorv1.ExecuteStart{}
	if c.Start != nil {
		start = proto.Clone(c.Start).(*connectorv1.ExecuteStart)
	}
	if start.ExecutionId == "" {
		start.ExecutionId = "contracttest-exec"
	}
	if start.StepId == "" {
		start.StepId = "contracttest-step"
	}
	if start.Connection == nil {
		start.Connection = o.Connection
	}
	send := func(m *connectorv1.ExecuteRequest) {
		if err := stream.Send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Start{Start: start}})
	size := c.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	for _, d := range c.Documents {
		for _, ch := range chunks(d, size) {
			send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: ch}})
		}
	}
	send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}})
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	results := map[string]*connectorv1.DocumentResult{}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Execute stream ended with a non-OK status: %v", err)
		}
		if r := resp.GetResult(); r != nil {
			if _, dup := results[r.GetDocumentId()]; dup {
				t.Errorf("more than one DocumentResult for document %q", r.GetDocumentId())
			}
			results[r.GetDocumentId()] = r
		}
	}

	input := map[string]bool{}
	for _, d := range c.Documents {
		input[d.ID] = true
		r, ok := results[d.ID]
		if !ok {
			t.Errorf("no DocumentResult for document %q", d.ID)
			continue
		}
		if r.GetStatus() == connectorv1.DocumentStatus_DOCUMENT_STATUS_UNSPECIFIED {
			t.Errorf("document %q: status is unspecified", d.ID)
		}
		if want, ok := c.Want[d.ID]; ok && r.GetStatus() != want {
			t.Errorf("document %q: status %v, want %v", d.ID, r.GetStatus(), want)
		}
	}
	for id := range results {
		if !input[id] {
			t.Errorf("DocumentResult for unknown document %q", id)
		}
	}
}

func chunks(d Document, size int) []*connectorv1.DocumentChunk {
	if len(d.Data) == 0 {
		return []*connectorv1.DocumentChunk{{DocumentId: d.ID, Properties: d.Properties, Last: true}}
	}
	var out []*connectorv1.DocumentChunk
	for off := 0; off < len(d.Data); off += size {
		end := min(off+size, len(d.Data))
		ch := &connectorv1.DocumentChunk{DocumentId: d.ID, Data: d.Data[off:end], Last: end == len(d.Data)}
		if off == 0 {
			ch.Properties = d.Properties
		}
		out = append(out, ch)
	}
	return out
}
