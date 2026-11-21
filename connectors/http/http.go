// SPDX-License-Identifier: Apache-2.0

// Package http is the native HTTP/REST connector.
package http

import (
	"context"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Connector implements connector.Connector for HTTP/REST.
type Connector struct{}

var _ connector.Connector = (*Connector)(nil)

// New returns the HTTP connector.
func New() *Connector { return &Connector{} }

func (*Connector) Describe(context.Context) (*connectorv1.Descriptor, error) {
	return &connectorv1.Descriptor{
		Id:          "http",
		Version:     "0.1.0",
		Host:        connectorv1.Host_HOST_GO_NATIVE,
		DisplayName: "HTTP / REST",
		Operations: []*connectorv1.OperationDescriptor{
			{Type: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: "REQUEST", SupportsBrowse: true},
			{Type: connectorv1.OperationType_OPERATION_TYPE_LISTEN},
		},
		TrackedProperties: []string{"status_code", "url"},
	}, nil
}

func (*Connector) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented // TODO(M1): OpenAPI import -> profiles
}

func (*Connector) Execute(connectorv1.ConnectorService_ExecuteServer) error {
	return connector.ErrNotImplemented // TODO(M1): streamed requests, per-document results
}

func (*Connector) Test(context.Context, *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return &connectorv1.TestResponse{Ok: false, Diagnostic: "not implemented"}, nil
}
