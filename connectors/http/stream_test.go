// SPDX-License-Identifier: Apache-2.0

package http_test

import (
	"context"
	"io"
	"testing"

	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
)

// memStream is an in-memory connectorv1.ConnectorService_ExecuteServer.
type memStream struct {
	grpc.ServerStream
	t   *testing.T
	in  []*connectorv1.ExecuteRequest
	res map[string]*connectorv1.DocumentResult
	out map[string][]byte
}

func (m *memStream) Context() context.Context { return m.t.Context() }

func (m *memStream) Recv() (*connectorv1.ExecuteRequest, error) {
	if len(m.in) == 0 {
		return nil, io.EOF
	}
	r := m.in[0]
	m.in = m.in[1:]
	return r, nil
}

func (m *memStream) Send(r *connectorv1.ExecuteResponse) error {
	if x := r.GetResult(); x != nil {
		m.res[x.GetDocumentId()] = x
	}
	if o := r.GetOutput(); o != nil {
		m.out[o.GetDocumentId()] = append(m.out[o.GetDocumentId()], o.GetData()...)
	}
	return nil
}
