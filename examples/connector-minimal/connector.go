// SPDX-License-Identifier: Apache-2.0

// Package minimal is the smallest useful Nurtuo connector: it upper-cases document payloads.
// See docs/connector-authoring.md.
//
// Operation: CREATE. Every input document yields one output document "<id>.out" holding the
// upper-cased payload. A document with an empty payload is rejected with APPLICATION_ERROR (the
// "target system" refused it); a payload that is not valid UTF-8 is a FAILURE.
package minimal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Version is the connector version reported in the descriptor (SemVer).
const Version = "0.1.0"

// Connector implements connector.Connector.
type Connector struct{}

var _ connector.Connector = (*Connector)(nil)

// New returns the connector.
func New() *Connector { return &Connector{} }

// Describe returns the descriptor: identity, host, operations and tracked properties.
func (*Connector) Describe(context.Context) (*connectorv1.Descriptor, error) {
	return &connectorv1.Descriptor{
		Id:          "uppercase",
		Version:     Version,
		Host:        connectorv1.Host_HOST_GO_NATIVE,
		DisplayName: "Uppercase (example)",
		Operations: []*connectorv1.OperationDescriptor{
			{Type: connectorv1.OperationType_OPERATION_TYPE_CREATE},
		},
		TrackedProperties: []string{"bytes"},
		Capabilities:      &connectorv1.Capabilities{Streaming: false},
	}, nil
}

// Browse is optional; return connector.ErrNotImplemented when there is nothing to browse.
func (*Connector) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}

// Test is "test connection". This connector has no external system, so it always succeeds.
// A real connector must never put secrets into the diagnostic.
func (*Connector) Test(context.Context, *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return &connectorv1.TestResponse{Ok: true, Diagnostic: "nothing to connect to"}, nil
}

type document struct {
	id   string
	data bytes.Buffer
}

// Execute follows the stream protocol of connector.proto: ExecuteStart first, then chunks of
// documents (the last chunk of a document has Last=true), then ExecuteEnd. For every input
// document the server must send exactly one DocumentResult; per-document problems are results,
// never gRPC errors.
func (*Connector) Execute(stream connectorv1.ConnectorService_ExecuteServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetStart() == nil {
		return errors.New("uppercase: first message must be ExecuteStart")
	}

	var order []*document
	byID := map[string]*document{}
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
		d, ok := byID[ch.GetDocumentId()]
		if !ok {
			d = &document{id: ch.GetDocumentId()}
			byID[d.id] = d
			order = append(order, d)
		}
		d.data.Write(ch.GetData())
	}

	for _, d := range order {
		res := &connectorv1.DocumentResult{DocumentId: d.id}
		switch {
		case d.data.Len() == 0:
			res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
			res.Message = "empty payload"
		case !utf8.Valid(d.data.Bytes()):
			res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE
			res.Message = "payload is not valid UTF-8"
		default:
			out := bytes.ToUpper(d.data.Bytes())
			outID := d.id + ".out"
			err := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{
				Output: &connectorv1.DocumentChunk{DocumentId: outID, Data: out, Last: true},
			}})
			if err != nil {
				return err
			}
			res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
			res.OutputDocumentIds = []string{outID}
			res.Tracked = map[string]string{"bytes": strconv.Itoa(len(out))}
		}
		if err := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: res}}); err != nil {
			return err
		}
	}
	return nil
}
