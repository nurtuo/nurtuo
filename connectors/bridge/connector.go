// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Connector returns the bridge as a connector.Connector, so the runtime can treat a bridged
// connector exactly like a native one (and so it can be re-served or tested with
// contracttest.Options.New).
func (b *Bridge) Connector() connector.Connector {
	return &client{cl: b.Client()}
}

type client struct {
	cl connectorv1.ConnectorServiceClient
}

func (c *client) Describe(ctx context.Context) (*connectorv1.ConnectorDescriptor, error) {
	resp, err := c.cl.Describe(ctx, &connectorv1.DescribeRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetDescriptor_() == nil {
		return nil, errors.New("bridge: Describe returned no descriptor")
	}
	return resp.GetDescriptor_(), nil
}

func (c *client) Browse(ctx context.Context, req *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return c.cl.Browse(ctx, req)
}

func (c *client) Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return c.cl.Test(ctx, req)
}

// Execute proxies the server-side stream to a client stream on the bridge: requests flow from
// s to the bridge, responses (results and output chunks) flow back. A non-OK status of the
// bridge stream (including the bridge dying mid-batch) is returned as is, never as success.
func (c *client) Execute(s connectorv1.ConnectorService_ExecuteServer) error {
	ctx, cancel := context.WithCancel(s.Context())
	defer cancel()
	cs, err := c.cl.Execute(ctx)
	if err != nil {
		return err
	}
	sendErr := make(chan error, 1)
	go func() {
		for {
			req, err := s.Recv()
			if errors.Is(err, io.EOF) {
				sendErr <- cs.CloseSend()
				return
			}
			if err != nil {
				sendErr <- err
				cancel()
				return
			}
			if err := cs.Send(req); err != nil {
				// io.EOF: the bridge ended the stream; Recv below reports the real status.
				if errors.Is(err, io.EOF) {
					sendErr <- nil
				} else {
					sendErr <- err
				}
				return
			}
		}
	}()
	for {
		resp, err := cs.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := s.Send(resp); err != nil {
			return fmt.Errorf("bridge: forward response: %w", err)
		}
	}
	select {
	case err := <-sendErr:
		return err
	default:
		return nil // the bridge finished before we finished sending; its status was OK
	}
}
