// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	httpconnector "github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/connectors/postgres"
	"github.com/nurtuo/nurtuo/connectors/sftp"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
)

// Bound connector-owned buffers within the existing isolation threshold. The engine and
// supervisor separately enforce the 1 MiB and 20-document returned-result bounds.
const pinnedIntermediateBytes = 8 * 1024 * 1024

type dialPlan struct {
	HTTP     map[string]string `json:"http"`
	Postgres map[string]string `json:"postgres"`
}

var errUnpinnedDial = errors.New("destination is outside the test run dial plan")

// The control plane supplies the dial plan in a clean child environment. A distinct command
// makes an older runtime refuse the request before it can resolve secrets or open a socket.
func pinnedConnectors() (*connhost.Registry, error) {
	var plan dialPlan
	if err := json.Unmarshal([]byte(os.Getenv("NURTUO_TEST_RUN_DIAL_PLAN")), &plan); err != nil ||
		len(plan.HTTP)+len(plan.Postgres) == 0 {
		return nil, errUnpinnedDial
	}
	reg := connhost.NewRegistry()
	reg.Register("sftp", func() connector.Connector { return sftp.New() })
	reg.Register("http", func() connector.Connector {
		transport := &http.Transport{DialContext: func(ctx context.Context, network, logical string) (net.Conn, error) {
			destination, err := pinnedAddress(plan.HTTP, logical)
			if err != nil || network != "tcp" {
				return nil, errUnpinnedDial
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", destination)
		}}
		return &httpconnector.Connector{Client: &http.Client{
			Transport:     getOnlyTransport{transport},
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are disabled in test runs") },
		}}
	})
	reg.Register("postgres", func() connector.Connector {
		c := postgres.New()
		c.MaxQueryResultBytes = pinnedIntermediateBytes
		c.ConfigurePool = func(pc *pgxpool.Config) error {
			cfg := pc.ConnConfig
			host := cfg.Host
			destination, err := pinnedAddress(plan.Postgres, net.JoinHostPort(host, strconv.Itoa(int(cfg.Port))))
			if err != nil || len(cfg.Fallbacks) != 0 {
				return errUnpinnedDial
			}
			ip, _, err := net.SplitHostPort(destination)
			if err != nil {
				return err
			}
			cfg.LookupFunc = func(_ context.Context, proposed string) ([]string, error) {
				if proposed != host {
					return nil, errUnpinnedDial
				}
				return []string{ip}, nil
			}
			cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" || address != destination {
					return nil, errUnpinnedDial
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", destination)
			}
			// Extended protocol accepts one statement per execution. QUERY also opens a READ ONLY
			// transaction in the connector. The saved role governs access to database objects.
			cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
			cfg.MaxProtocolMessageBodyLen = 1024 * 1024
			return nil
		}
		return c
	})
	return reg, nil
}

func pinnedAddress(pins map[string]string, logical string) (string, error) {
	_, port, err := net.SplitHostPort(logical)
	if err != nil {
		return "", errUnpinnedDial
	}
	ip, err := netip.ParseAddr(pins[logical])
	if err != nil {
		return "", errUnpinnedDial
	}
	return net.JoinHostPort(ip.String(), port), nil
}

type getOnlyTransport struct{ *http.Transport }

func (transport getOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, errors.New("test runs allow HTTP GET only")
	}
	for _, name := range []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"} {
		if _, present := req.Header[name]; present {
			return nil, errors.New("HTTP method override headers are disabled in test runs")
		}
	}
	response, err := transport.Transport.RoundTrip(req)
	if err == nil {
		response.Body = &boundedResponseBody{response.Body, pinnedIntermediateBytes}
	}
	return response, err
}

type boundedResponseBody struct {
	io.ReadCloser
	remaining int
}

func (body *boundedResponseBody) Read(p []byte) (int, error) {
	// Read at most one byte over budget to distinguish overflow from an exact-size response.
	n, err := body.ReadCloser.Read(p[:min(len(p), body.remaining+1)])
	body.remaining -= n
	if body.remaining < 0 {
		body.remaining = 0
		return 0, errors.New("HTTP response exceeds the test run buffer limit")
	}
	return n, err
}
