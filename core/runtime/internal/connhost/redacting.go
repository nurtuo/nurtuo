// SPDX-License-Identifier: AGPL-3.0-only

package connhost

import (
	"context"

	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// redacting wraps a connector so a secret that a connector echoes by mistake (in an Execute error,
// a per-document message or a Test diagnostic) never reaches documents, logs or status messages.
type redacting struct {
	c connector.Connector
	r *secrets.Redactor
}

func (w *redacting) Describe(ctx context.Context) (*connectorv1.ConnectorDescriptor, error) {
	return w.c.Describe(ctx)
}

func (w *redacting) Browse(ctx context.Context, req *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	resp, err := w.c.Browse(ctx, req)
	return resp, w.r.RedactError(err)
}

func (w *redacting) Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	resp, err := w.c.Test(ctx, req)
	if resp != nil {
		resp.Diagnostic = w.r.Redact(resp.Diagnostic)
	}
	return resp, w.r.RedactError(err)
}

func (w *redacting) Execute(s connectorv1.ConnectorService_ExecuteServer) error {
	return w.r.RedactError(w.c.Execute(&redactStream{ConnectorService_ExecuteServer: s, r: w.r}))
}

type redactStream struct {
	connectorv1.ConnectorService_ExecuteServer
	r *secrets.Redactor
}

func (s *redactStream) Send(m *connectorv1.ExecuteResponse) error {
	if res := m.GetResult(); res != nil {
		res.Message = s.r.Redact(res.Message)
	}
	return s.ConnectorService_ExecuteServer.Send(m)
}
