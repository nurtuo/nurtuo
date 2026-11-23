// SPDX-License-Identifier: Apache-2.0

package contracttest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// fake is a trivial connector: a document whose payload contains "bad" is rejected with
// APPLICATION_ERROR, everything else succeeds.
type fake struct{}

func (fake) Describe(context.Context) (*connectorv1.ConnectorDescriptor, error) {
	return &connectorv1.ConnectorDescriptor{
		Id:          "fake",
		Version:     "0.1.0",
		Host:        connectorv1.Host_HOST_GO_NATIVE,
		DisplayName: "Fake",
		Operations:  []*connectorv1.OperationDescriptor{{Type: connectorv1.OperationType_OPERATION_TYPE_CREATE}},
	}, nil
}

func (fake) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}

func (fake) Test(context.Context, *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return &connectorv1.TestResponse{Ok: true, Diagnostic: "reachable"}, nil
}

func (fake) Execute(s connectorv1.ConnectorService_ExecuteServer) error {
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
			if _, seen := data[ch.GetDocumentId()]; !seen {
				order = append(order, ch.GetDocumentId())
			}
			data[ch.GetDocumentId()] = append(data[ch.GetDocumentId()], ch.GetData()...)
		}
	}
	for _, id := range order {
		st := connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
		if bytes.Contains(data[id], []byte("bad")) {
			st = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
		}
		res := &connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{
			Result: &connectorv1.DocumentResult{DocumentId: id, Status: st},
		}}
		if err := s.Send(res); err != nil {
			return err
		}
	}
	return nil
}

func TestFakeConnectorConforms(t *testing.T) {
	contracttest.Run(t, contracttest.Options{
		New: func(testing.TB) connector.Connector { return fake{} },
		Execute: []contracttest.ExecuteCase{{
			Name: "per-document status",
			Start: &connectorv1.ExecuteStart{
				OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE,
			},
			Documents: []contracttest.Document{
				{ID: "d1", Data: []byte("good")},
				{ID: "d2", Data: []byte("bad payload")},
				{ID: "d3", Data: bytes.Repeat([]byte("x"), 200_000)}, // multiple chunks
			},
			Want: map[string]connectorv1.DocumentStatus{
				"d1": connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
				"d2": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
				"d3": connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
			},
		}},
	})
}
