// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// stubConnector is a connector for the flow run tests. Its behavior comes from the connection
// settings: mode = ok (default) | fail | failure | block | two | leak; payload = the output bytes
// (default: the input document's "path" property); password is a secret setting.
type stubConnector struct {
	mu   sync.Mutex
	seen []map[string]string // properties of every input document
}

func stubRegistry() (*connhost.Registry, *stubConnector) {
	st := &stubConnector{}
	reg := connhost.NewRegistry()
	reg.Register("stub", func() connector.Connector { return st })
	return reg, st
}

func (s *stubConnector) Describe(context.Context) (*connectorv1.ConnectorDescriptor, error) {
	schema, _ := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"password": map[string]any{"type": "string", "writeOnly": true},
			"mode":     map[string]any{"type": "string"},
			"payload":  map[string]any{"type": "string"},
		},
	})
	return &connectorv1.ConnectorDescriptor{ConnectionSchema: schema}, nil
}

func (s *stubConnector) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}

func (s *stubConnector) Test(context.Context, *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return &connectorv1.TestResponse{Ok: true}, nil
}

func (s *stubConnector) Execute(stream connectorv1.ConnectorService_ExecuteServer) error {
	var settings map[string]any
	type in struct {
		id    string
		props map[string]string
	}
	var inputs []in
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if st := req.GetStart(); st != nil {
			settings = st.GetConnection().GetSettings().AsMap()
		}
		if ch := req.GetChunk(); ch != nil && ch.GetProperties() != nil {
			inputs = append(inputs, in{ch.GetDocumentId(), ch.GetProperties()})
		}
		if req.GetEnd() != nil {
			break
		}
	}
	str := func(k string) string { v, _ := settings[k].(string); return v }
	mode := str("mode")
	for _, d := range inputs {
		s.mu.Lock()
		s.seen = append(s.seen, d.props)
		s.mu.Unlock()
	}
	switch mode {
	case "fail":
		return errors.New("stub connector failed: cannot connect")
	case "leak":
		return errors.New("login failed for password " + str("password"))
	case "block":
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	for _, d := range inputs {
		if mode == "failure" {
			_ = stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: &connectorv1.DocumentResult{
				DocumentId: d.id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, Message: "no such file: " + d.props["path"],
			}}})
			continue
		}
		payload := str("payload")
		if _, set := settings["payload"]; !set {
			payload = d.props["path"]
		}
		outs := []string{d.id + ".out"}
		if mode == "two" {
			outs = append(outs, d.id+".out2")
		}
		for _, o := range outs {
			_ = stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{Output: &connectorv1.DocumentChunk{
				DocumentId: o, Data: []byte(payload), Last: true, Properties: map[string]string{"filename": "f.txt"},
			}}})
		}
		_ = stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: &connectorv1.DocumentResult{
			DocumentId: d.id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS, OutputDocumentIds: outs,
		}}})
	}
	return nil
}

// settingsJSON builds a stub connection settings object.
func stubSettings(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, `"`+kv[i]+`":`+kv[i+1])
	}
	return "{" + strings.Join(parts, ",") + "}"
}
