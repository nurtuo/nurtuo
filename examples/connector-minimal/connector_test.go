// SPDX-License-Identifier: Apache-2.0

package minimal_test

import (
	"testing"

	minimal "github.com/nurtuo/nurtuo/examples/connector-minimal"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// TestContract runs the shared conformance suite against the connector through the real gRPC
// server (connector.Serve) over an in-memory listener.
func TestContract(t *testing.T) {
	contracttest.Run(t, contracttest.Options{
		New: func(testing.TB) connector.Connector { return minimal.New() },
		Execute: []contracttest.ExecuteCase{{
			Name:  "per-document status",
			Start: &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE},
			Documents: []contracttest.Document{
				{ID: "ok", Data: []byte("hello")},
				{ID: "empty"},
				{ID: "binary", Data: []byte{0xff, 0xfe}},
				{ID: "large", Data: make([]byte, 200_000)}, // several chunks
			},
			Want: map[string]connectorv1.DocumentStatus{
				"ok":     connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
				"empty":  connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
				"binary": connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
				"large":  connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
			},
		}},
	})
}
