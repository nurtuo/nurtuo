// SPDX-License-Identifier: Apache-2.0

// Package nats is the native NATS connector (core NATS and JetStream).
//
// Operations:
//   - CREATE: core NATS publish (fire and forget).
//   - EXECUTE custom_type "REQUEST": request/reply; the reply is returned as an output document.
//   - EXECUTE custom_type "JETSTREAM_PUBLISH": publish and wait for the JetStream ack.
//
// The subject comes from the document property "nats.subject" or, if absent, from the step
// option "subject". Document properties prefixed "nats.header." become NATS message headers.
//
// Listen (subscribe with ack/nack) is not implemented: the Go connector SDK interface does not
// expose Listen yet (connector.Connector has no Listen method and connector.Serve does not
// register it).
package nats

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// ID is the connector id.
	ID      = "nats"
	Version = "0.1.0"

	CustomRequest      = "REQUEST"
	CustomJetStreamPub = "JETSTREAM_PUBLISH"

	// PropSubject overrides the subject per document.
	PropSubject = "nats.subject"
	// PropHeaderPrefix marks document properties that are sent as NATS headers.
	PropHeaderPrefix = "nats.header."

	defaultRequestTimeout = 5 * time.Second

	// Execute buffers the batch before connecting; these bound that buffer.
	maxBatchBytes = 64 << 20
	maxBatchDocs  = 100_000
)

// Connector implements connector.Connector.
type Connector struct{}

var _ connector.Connector = (*Connector)(nil)

// New returns a NATS connector.
func New() *Connector { return &Connector{} }

// Describe returns the connector descriptor.
func (*Connector) Describe(context.Context) (*connectorv1.Descriptor, error) {
	schema, err := structpb.NewStruct(map[string]any{
		"type":     "object",
		"required": []any{"url"},
		"properties": map[string]any{
			"url":                map[string]any{"type": "string", "title": "Server URL(s)", "description": "nats://host:4222, comma separated for a cluster"},
			"token":              map[string]any{"type": "string", "format": "secret"},
			"username":           map[string]any{"type": "string"},
			"password":           map[string]any{"type": "string", "format": "secret"},
			"creds":              map[string]any{"type": "string", "format": "secret", "description": "Content of a .creds file"},
			"nkey_seed":          map[string]any{"type": "string", "format": "secret"},
			"name":               map[string]any{"type": "string"},
			"connect_timeout_ms": map[string]any{"type": "integer", "minimum": 1},
			"tls": map[string]any{"type": "object", "properties": map[string]any{
				"enabled":              map[string]any{"type": "boolean"},
				"ca_pem":               map[string]any{"type": "string"},
				"cert_pem":             map[string]any{"type": "string"},
				"key_pem":              map[string]any{"type": "string", "format": "secret"},
				"server_name":          map[string]any{"type": "string"},
				"insecure_skip_verify": map[string]any{"type": "boolean"},
			}},
		},
	})
	if err != nil {
		return nil, err
	}
	pubOpts, err := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subject":    map[string]any{"type": "string", "description": "Default subject when a document has no nats.subject property"},
			"timeout_ms": map[string]any{"type": "integer", "minimum": 1, "description": "Request/ack timeout"},
		},
	})
	if err != nil {
		return nil, err
	}
	return &connectorv1.Descriptor{
		Id:               ID,
		Version:          Version,
		Host:             connectorv1.Host_HOST_GO_NATIVE,
		DisplayName:      "NATS",
		ConnectionSchema: schema,
		Operations: []*connectorv1.OperationDescriptor{
			{Type: connectorv1.OperationType_OPERATION_TYPE_CREATE, OptionsSchema: pubOpts},
			{Type: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: CustomRequest, OptionsSchema: pubOpts},
			{Type: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: CustomJetStreamPub, OptionsSchema: pubOpts},
		},
		TrackedProperties: []string{"subject", "stream", "sequence"},
		Capabilities:      &connectorv1.Capabilities{Streaming: false, MaxBatchSize: 0},
	}, nil
}

// Browse is not supported.
func (*Connector) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}

// Test connects to the server and, on success, reports server and JetStream availability.
func (*Connector) Test(parent context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	cfg, err := parseConfig(req.GetConnection())
	if err != nil {
		return &connectorv1.TestResponse{Ok: false, Diagnostic: err.Error()}, nil
	}
	nc, err := cfg.connectCtx(parent)
	if err != nil {
		return &connectorv1.TestResponse{Ok: false, Diagnostic: redact(err.Error(), cfg)}, nil
	}
	defer nc.Close()
	diag := "connected to NATS " + nc.ConnectedServerVersion()
	if js, err := jetstream.New(nc); err == nil {
		ctx, cancel := context.WithTimeout(parent, cfg.ConnectTimeout)
		defer cancel()
		if _, err := js.AccountInfo(ctx); err == nil {
			diag += "; JetStream available"
		} else {
			diag += "; JetStream not available"
		}
	}
	return &connectorv1.TestResponse{Ok: true, Diagnostic: diag}, nil
}

type doc struct {
	id    string
	props map[string]string
	data  []byte
}

// Execute implements the Execute stream: it buffers the batch, then processes documents in
// order, returning one DocumentResult each. Connection-level problems fail the whole stream;
// per-document problems are reported per document.
func (*Connector) Execute(stream connectorv1.ConnectorService_ExecuteServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "first message must be ExecuteStart")
	}
	cfg, err := parseConfig(start.GetConnection())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	opType, custom := start.GetOperationType(), start.GetCustomType()
	switch {
	case opType == connectorv1.OperationType_OPERATION_TYPE_CREATE:
	case opType == connectorv1.OperationType_OPERATION_TYPE_EXECUTE && (custom == CustomRequest || custom == CustomJetStreamPub):
	default:
		return status.Errorf(codes.Unimplemented, "unsupported operation %v %q", opType, custom)
	}
	var defSubject string
	timeout := defaultRequestTimeout
	if o := start.GetOptions().AsMap(); o != nil {
		defSubject, _ = o["subject"].(string)
		if v, ok := o["timeout_ms"].(float64); ok && v > 0 {
			timeout = time.Duration(v) * time.Millisecond
		}
	}

	var order []string
	buffered := 0
	docs := map[string]*doc{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if req.GetEnd() != nil {
			break
		}
		ch := req.GetChunk()
		if ch == nil {
			continue
		}
		d, ok := docs[ch.GetDocumentId()]
		if !ok {
			d = &doc{id: ch.GetDocumentId(), props: map[string]string{}}
			docs[d.id] = d
			order = append(order, d.id)
		}
		for k, v := range ch.GetProperties() {
			d.props[k] = v
		}
		buffered += len(ch.GetData())
		if buffered > maxBatchBytes || len(docs) > maxBatchDocs {
			return status.Errorf(codes.ResourceExhausted, "batch too large: more than %d bytes or %d documents buffered; send smaller batches", maxBatchBytes, maxBatchDocs)
		}
		d.data = append(d.data, ch.GetData()...)
	}

	nc, err := cfg.connectCtx(stream.Context())
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	defer nc.Close()
	var js jetstream.JetStream
	if custom == CustomJetStreamPub {
		if js, err = jetstream.New(nc); err != nil {
			return status.Error(codes.Unavailable, redact(err.Error(), cfg))
		}
	}

	for _, id := range order {
		d := docs[id]
		res := &connectorv1.DocumentResult{DocumentId: id}
		var out []byte
		subject := d.props[PropSubject]
		if subject == "" {
			subject = defSubject
		}
		var badHeader string
		for k, v := range d.props {
			if h, ok := strings.CutPrefix(k, PropHeaderPrefix); ok && h != "" &&
				(strings.ContainsAny(h, "\r\n:") || strings.ContainsAny(v, "\r\n")) {
				badHeader = h
			}
		}
		switch {
		case subject == "" || strings.ContainsAny(subject, " \t\r\n"):
			res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
			res.Message = "missing or invalid subject (set document property nats.subject or option subject)"
		case strings.ContainsAny(subject, "*>"):
			res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
			res.Message = "subject must not contain wildcards (* or >) when publishing"
		case badHeader != "":
			res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
			res.Message = "invalid header " + strconv.Quote(badHeader) + ": names and values must not contain CR or LF (names also not ':')"
		default:
			msg := &nats.Msg{Subject: subject, Data: d.data}
			for k, v := range d.props {
				if h, ok := strings.CutPrefix(k, PropHeaderPrefix); ok && h != "" {
					if msg.Header == nil {
						msg.Header = nats.Header{}
					}
					msg.Header.Set(h, v)
				}
			}
			if custom == CustomJetStreamPub && msg.Header.Get(nats.MsgIdHdr) == "" {
				if msg.Header == nil {
					msg.Header = nats.Header{}
				}
				msg.Header.Set(nats.MsgIdHdr, id) // retries of the same document dedupe in the stream
			}
			ctx, cancel := context.WithTimeout(stream.Context(), timeout)
			out = send(ctx, nc, js, custom, msg, res)
			cancel()
		}
		if out != nil {
			res.OutputDocumentIds = []string{id + "-reply"}
		}
		if err := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: res}}); err != nil {
			return err
		}
		if out != nil {
			o := &connectorv1.DocumentChunk{DocumentId: id + "-reply", Data: out, Last: true}
			if err := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{Output: o}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// send performs one operation, fills res and returns reply data for request/reply.
func send(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, custom string, msg *nats.Msg, res *connectorv1.DocumentResult) []byte {
	ok := func() { res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS }
	appErr := func(m string) {
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
		res.Message = m
	}
	fail := func(err error) {
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE
		res.Message = err.Error()
	}
	res.Tracked = map[string]string{"subject": msg.Subject}
	switch custom {
	case CustomRequest:
		reply, err := nc.RequestMsgWithContext(ctx, msg)
		switch {
		case err == nil:
			ok()
			return append([]byte{}, reply.Data...)
		case errors.Is(err, nats.ErrNoResponders):
			appErr("no responders for subject")
		default:
			fail(err)
		}
	case CustomJetStreamPub:
		ack, err := js.PublishMsg(ctx, msg)
		switch {
		case err == nil:
			ok()
			res.Tracked["stream"] = ack.Stream
			res.Tracked["sequence"] = strconv.FormatUint(ack.Sequence, 10)
		case errors.Is(err, jetstream.ErrNoStreamResponse):
			appErr("no JetStream stream captures subject")
		default:
			fail(err)
		}
	default: // CREATE
		if err := nc.PublishMsg(msg); err != nil {
			fail(err)
			break
		}
		if err := nc.FlushWithContext(ctx); err != nil {
			fail(err)
			break
		}
		ok()
	}
	return nil
}
