// SPDX-License-Identifier: Apache-2.0

// Package connector is the Go connector SDK: implement Connector, then either register it
// in the runtime (native connector) or call Serve to run it as an out-of-process connector.
package connector

import (
	"context"
	"errors"
	"net"

	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
)

// Connector is the language-neutral contract (describe / browse / execute / test) in Go form.
type Connector interface {
	Describe(ctx context.Context) (*connectorv1.ConnectorDescriptor, error)
	Browse(ctx context.Context, req *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error)
	Execute(stream connectorv1.ConnectorService_ExecuteServer) error
	Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error)
}

// ErrNotImplemented is returned by connectors for optional operations they do not support.
var ErrNotImplemented = errors.New("connector: not implemented")

type server struct {
	connectorv1.UnimplementedConnectorServiceServer
	c Connector
}

func (s *server) Describe(ctx context.Context, _ *connectorv1.DescribeRequest) (*connectorv1.DescribeResponse, error) {
	d, err := s.c.Describe(ctx)
	if err != nil {
		return nil, err
	}
	return &connectorv1.DescribeResponse{Descriptor_: d}, nil
}

func (s *server) Browse(ctx context.Context, req *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return s.c.Browse(ctx, req)
}

func (s *server) Execute(stream connectorv1.ConnectorService_ExecuteServer) error {
	return s.c.Execute(stream)
}

func (s *server) Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return s.c.Test(ctx, req)
}

// Serve runs c as a gRPC connector server on l (a Unix socket or named pipe listener handed
// over by the runtime during the handshake). It blocks until l is closed.
func Serve(l net.Listener, c Connector) error {
	g := grpc.NewServer()
	connectorv1.RegisterConnectorServiceServer(g, &server{c: c})
	return g.Serve(l)
}
