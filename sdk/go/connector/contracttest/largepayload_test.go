// SPDX-License-Identifier: Apache-2.0

package contracttest

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// sink is a connector that handles only Execute. With buffer set it keeps every chunk in memory
// until the stream ends; otherwise it only counts bytes.
type sink struct {
	connector.Connector
	buffer bool
	// failFirst sends a FAILURE result before the final SUCCESS for the same document.
	failFirst bool
}

func (s sink) Execute(st connectorv1.ConnectorService_ExecuteServer) error {
	var held []byte
	var id string
	for {
		req, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if ch := req.GetChunk(); ch != nil {
			id = ch.GetDocumentId()
			if s.buffer {
				held = append(held, ch.GetData()...)
			}
		}
	}
	_ = held
	if s.failFirst {
		if err := st.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{
			Result: &connectorv1.DocumentResult{DocumentId: id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE},
		}}); err != nil {
			return err
		}
	}
	return st.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{
		Result: &connectorv1.DocumentResult{DocumentId: id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS},
	}})
}

func TestLargePayloadMeasuresHeapGrowth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		buffer    bool
		overLimit bool
	}{
		{"streaming connector stays within the bound", false, false},
		{"buffering connector exceeds the bound", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &Options{Timeout: 30 * time.Second, New: func(testing.TB) connector.Connector { return sink{buffer: tc.buffer} }}
			c := LargePayloadCase{}.withDefaults()
			res, growth, err := streamLargePayload(t, o, c)
			if err != nil {
				t.Fatal(err)
			}
			if res.GetStatus() != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
				t.Fatalf("status %v", res.GetStatus())
			}
			if got := growth > c.MaxHeapGrowth; got != tc.overLimit {
				t.Errorf("growth %d bytes, limit %d: over limit = %v, want %v", growth, c.MaxHeapGrowth, got, tc.overLimit)
			}
		})
	}
}

func TestLargePayloadRejectsDuplicateResults(t *testing.T) {
	o := &Options{Timeout: 30 * time.Second, New: func(testing.TB) connector.Connector { return sink{failFirst: true} }}
	c := LargePayloadCase{}.withDefaults()
	if _, _, err := streamLargePayload(t, o, c); err == nil {
		t.Fatal("FAILURE followed by SUCCESS for one document was accepted, want an error")
	}
}
