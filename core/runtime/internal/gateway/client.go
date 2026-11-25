// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
)

// Handler is what the gateway client drives. The supervisor implements it.
type Handler interface {
	// Revision is the last fully applied desired-state revision (sent in Hello).
	Revision() uint64
	// ApplyDesiredState reconciles to ds, emitting status transitions through report.
	ApplyDesiredState(ctx context.Context, ds *gatewayv1.DesiredState, report func(*gatewayv1.DeploymentStatus)) error
	// HandleCommand executes a control-plane command. Unknown commands must return ok=false.
	HandleCommand(ctx context.Context, cmd *gatewayv1.Command) *gatewayv1.CommandResult
}

// Backoff is exponential with jitter.
type Backoff struct {
	Initial, Max time.Duration
	Factor       float64
}

// Next returns the delay before retry number attempt (0-based), with up to 20% jitter.
func (b Backoff) Next(attempt int) time.Duration {
	d := float64(b.Initial)
	for i := 0; i < attempt && d < float64(b.Max); i++ {
		d *= b.Factor
	}
	if d > float64(b.Max) {
		d = float64(b.Max)
	}
	return time.Duration(d * (1 - 0.2*rand.Float64()))
}

// Config configures the outbound client. The runtime always dials out; it never listens.
type Config struct {
	Target     string // control plane gateway address
	RuntimeID  string // must match the mTLS client certificate identity
	Version    string
	TLS        credentialsProvider // mTLS credentials; see MTLSCredentials
	DialOpts   []grpc.DialOption   // extra options (tests inject a bufconn dialer + insecure/TLS creds)
	Heartbeat  time.Duration       // default 15s
	Backoff    Backoff             // default 1s..60s x2
	Handler    Handler
	Logger     *slog.Logger
	OutboxSize int // buffered upstream messages, default 64
}

type credentialsProvider = credentials.TransportCredentials

// Client keeps one long-lived Connect stream to the control plane, reconnecting with backoff.
type Client struct {
	cfg    Config
	outbox chan *gatewayv1.ConnectRequest
	log    *slog.Logger

	connMu sync.Mutex
	conn   *grpc.ClientConn
}

func NewClient(cfg Config) *Client {
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.Backoff.Initial == 0 {
		cfg.Backoff = Backoff{Initial: time.Second, Max: time.Minute, Factor: 2}
	}
	if cfg.OutboxSize == 0 {
		cfg.OutboxSize = 64
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{cfg: cfg, outbox: make(chan *gatewayv1.ConnectRequest, cfg.OutboxSize), log: log}
}

// ReportStatus queues a deployment status upstream (telemetry stub). Best effort: if the outbox
// is full the message is dropped rather than blocking the engine.
func (c *Client) ReportStatus(s *gatewayv1.DeploymentStatus) {
	c.enqueue(&gatewayv1.ConnectRequest{Msg: &gatewayv1.ConnectRequest_DeploymentStatus{DeploymentStatus: s}})
}

func (c *Client) enqueue(m *gatewayv1.ConnectRequest) {
	select {
	case c.outbox <- m:
	default:
		c.log.Warn("gateway outbox full, dropping message")
	}
}

// Run connects and serves until ctx is cancelled. While disconnected the runtime keeps running
// its last applied state; Run just retries with backoff.
func (c *Client) Run(ctx context.Context) error {
	opts := append([]grpc.DialOption{}, c.cfg.DialOpts...)
	if c.cfg.TLS != nil {
		opts = append(opts, grpc.WithTransportCredentials(c.cfg.TLS))
	}
	conn, err := grpc.NewClient(c.cfg.Target, opts...)
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		c.conn = nil
		c.connMu.Unlock()
		_ = conn.Close()
	}()
	api := gatewayv1.NewRuntimeGatewayServiceClient(conn)

	attempt := 0
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx, api)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > 30*time.Second {
			attempt = 0 // a healthy session resets backoff
		}
		delay := c.cfg.Backoff.Next(attempt)
		attempt++
		c.log.Warn("gateway session ended, reconnecting", "err", err, "in", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

const fetchArtifactTimeout = 5 * time.Minute

// FetchArtifact streams an artifact over the connection owned by Run.
func (c *Client) FetchArtifact(ctx context.Context, digest string, w io.Writer, limit int64) (int64, error) {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return 0, errors.New("gateway: not connected")
	}

	ctx, cancel := context.WithTimeout(ctx, fetchArtifactTimeout)
	defer cancel()
	stream, err := gatewayv1.NewRuntimeGatewayServiceClient(conn).FetchArtifact(ctx, &gatewayv1.FetchArtifactRequest{Digest: digest})
	if err != nil {
		return 0, err
	}
	var total int64
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		data := chunk.GetData()
		if int64(len(data)) > limit-total {
			return total, fmt.Errorf("artifact larger than %d bytes", limit)
		}
		n, err := w.Write(data)
		total += int64(n)
		if err != nil {
			return total, err
		}
		if n != len(data) {
			return total, io.ErrShortWrite
		}
	}
}

func (c *Client) session(ctx context.Context, api gatewayv1.RuntimeGatewayServiceClient) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := api.Connect(ctx)
	if err != nil {
		return err
	}
	hello := &gatewayv1.ConnectRequest{Msg: &gatewayv1.ConnectRequest_Hello{Hello: &gatewayv1.Hello{
		RuntimeId:            c.cfg.RuntimeID,
		Version:              c.cfg.Version,
		Os:                   runtime.GOOS,
		Arch:                 runtime.GOARCH,
		DesiredStateRevision: c.cfg.Handler.Revision(),
	}}}
	if err := stream.Send(hello); err != nil {
		return err
	}

	// Single sender goroutine: gRPC streams allow one concurrent Send.
	sendErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(c.cfg.Heartbeat)
		defer t.Stop()
		for {
			var m *gatewayv1.ConnectRequest
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m = &gatewayv1.ConnectRequest{Msg: &gatewayv1.ConnectRequest_Heartbeat{Heartbeat: &gatewayv1.Heartbeat{At: timestamppb.Now()}}}
			case m = <-c.outbox:
			}
			if err := stream.Send(m); err != nil {
				sendErr <- err
				return
			}
		}
	}()

	report := func(s *gatewayv1.DeploymentStatus) { c.ReportStatus(s) }

	// Applies run in a worker so a slow one (for example strict connection tests) never blocks
	// the receive loop, which must keep reading commands. Single "latest wins" slot: a newer
	// desired state replaces one that has not started yet; there is no backlog.
	var (
		slotMu sync.Mutex
		slot   *gatewayv1.DesiredState
	)
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			slotMu.Lock()
			ds := slot
			slot = nil
			slotMu.Unlock()
			if ds == nil {
				continue
			}
			if err := c.cfg.Handler.ApplyDesiredState(ctx, ds, report); err != nil {
				c.log.Error("apply desired state", "revision", ds.GetRevision(), "err", err)
			}
		}
	}()
	// Cancel and wait so applies of consecutive sessions never overlap. Runs before the
	// deferred cancel registered above, hence the explicit cancel.
	defer func() { cancel(); wg.Wait() }()

	for {
		resp, err := stream.Recv()
		if err != nil {
			select {
			case se := <-sendErr:
				err = se
			default:
			}
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return err
		}
		switch m := resp.GetMsg().(type) {
		case *gatewayv1.ConnectResponse_DesiredState:
			slotMu.Lock()
			slot = m.DesiredState
			slotMu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
		case *gatewayv1.ConnectResponse_Command:
			res := c.cfg.Handler.HandleCommand(ctx, m.Command)
			if res == nil {
				res = &gatewayv1.CommandResult{Ok: false, Message: "no result"}
			}
			res.CommandId = m.Command.GetCommandId()
			c.enqueue(&gatewayv1.ConnectRequest{Msg: &gatewayv1.ConnectRequest_CommandResult{CommandResult: res}})
		}
	}
}
