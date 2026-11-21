// SPDX-License-Identifier: Apache-2.0

// Package bridge launches and supervises out-of-process connector hosts (the JVM, Python and
// .NET bridges under bridges/) and exposes them as a gRPC client of the connector contract
// (nurtuo.connector.v1).
//
// Lifecycle:
//
//  1. Start runs the configured command with NURTUO_BRIDGE_* environment variables
//     (listen address, accepted contracts, shared token) and waits, up to Config.StartTimeout,
//     for the handshake line on the child's stdout (see Handshake and bridges/README.md).
//  2. The bridge is dialed lazily through a custom dialer that always resolves to the current
//     child, so the *grpc.ClientConn from Conn survives restarts. RPCs issued while the child
//     is restarting wait for it (grpc.WaitForReady), bounded by the caller's context.
//  3. If the child exits unexpectedly it is restarted with exponential backoff, at most
//     Config.MaxRestarts times per Config.RestartWindow. When the budget is exhausted the
//     bridge is marked failed: Done is closed, Err reports why and RPCs fail fast.
//  4. Close asks the child to stop by closing its stdin (portable "the runtime is going away"
//     signal, honoured by all bridges), waits Config.StopTimeout, then kills it.
//
// The package depends only on sdk/go (Apache-2.0), never on core/.
package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Config describes how to run a bridge.
type Config struct {
	// Command and Args start the bridge, e.g. "python", "-m", "nurtuo_bridge", "pkg.mod:Class".
	Command string
	Args    []string
	// Dir is the working directory of the child (default: the runtime's).
	Dir string
	// Env are extra "KEY=value" variables. The child does NOT inherit the runtime's
	// environment (it may hold secrets); only a small allowlist (PATH, temp dirs, home,
	// locale, SystemRoot...) is passed, then Env, then the NURTUO_BRIDGE_* variables.
	Env []string
	// Network is "unix", "tcp" (loopback) or "auto" (default: unix, except tcp on Windows).
	Network string
	// Contracts are the acceptable connector contract packages in order of preference
	// (default: nurtuo.connector.v1). The bridge picks one it supports and echoes it in the
	// handshake; if it supports none it exits with code 3.
	Contracts []string
	// StartTimeout bounds the wait for the handshake (default 15s).
	StartTimeout time.Duration
	// StopTimeout is how long Close waits for the child to exit after closing its stdin
	// before killing it (default 5s).
	StopTimeout time.Duration
	// MaxRestarts is the number of restarts allowed within RestartWindow (default 3; a
	// negative value disables restarts).
	MaxRestarts int
	// RestartWindow is the sliding window for MaxRestarts (default 1 minute).
	RestartWindow time.Duration
	// RestartBackoff is the delay before the first restart, doubled for each further restart
	// in the window and capped at 5s (default 200ms).
	RestartBackoff time.Duration
	// Stderr receives the child's stderr (default os.Stderr).
	Stderr io.Writer
	// Logger receives supervision events (default slog.Default()).
	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Network == "" || c.Network == "auto" {
		c.Network = "unix"
		if runtime.GOOS == "windows" {
			c.Network = "tcp"
		}
	}
	if len(c.Contracts) == 0 {
		c.Contracts = []string{ContractV1}
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 15 * time.Second
	}
	if c.StopTimeout <= 0 {
		c.StopTimeout = 5 * time.Second
	}
	if c.MaxRestarts == 0 {
		c.MaxRestarts = 3
	}
	if c.RestartWindow <= 0 {
		c.RestartWindow = time.Minute
	}
	if c.RestartBackoff <= 0 {
		c.RestartBackoff = 200 * time.Millisecond
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

var errClosed = errors.New("bridge: closed")

// Bridge is a running, supervised bridge process and a client connection to it.
type Bridge struct {
	cfg   Config
	log   *slog.Logger
	token string
	cc    *grpc.ClientConn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	cur      *proc         // nil while restarting or after failure
	ready    chan struct{} // closed when cur is set; replaced when the child dies
	restarts int
	err      error // terminal failure

	restartTimes []time.Time // supervisor goroutine only
	closed       bool

	done     chan struct{} // closed on Close or terminal failure
	doneOnce sync.Once
}

// Start launches the bridge and waits for its handshake. ctx bounds only the initial start.
func Start(ctx context.Context, cfg Config) (*Bridge, error) {
	if cfg.Command == "" {
		return nil, errors.New("bridge: Config.Command is required")
	}
	cfg = cfg.withDefaults()
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return nil, fmt.Errorf("bridge: token: %w", err)
	}
	bctx, cancel := context.WithCancel(context.Background())
	b := &Bridge{
		cfg:    cfg,
		log:    cfg.Logger.With("bridge", filepath.Base(cfg.Command)),
		token:  hex.EncodeToString(tok),
		ctx:    bctx,
		cancel: cancel,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	p, err := b.launch(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	b.setCurrent(p)
	cc, err := b.NewConn()
	if err != nil {
		p.stop(cfg.StopTimeout)
		cancel()
		return nil, fmt.Errorf("bridge: client: %w", err)
	}
	b.cc = cc
	b.wg.Add(1)
	go b.supervise(p)
	return b, nil
}

// Conn is the client connection to the bridge. It stays valid across restarts. Pass it to
// contracttest.Options.Dial or wrap it with connectorv1.NewConnectorServiceClient.
func (b *Bridge) Conn() *grpc.ClientConn { return b.cc }

// NewConn opens an additional client connection to the bridge (same dialer, token and
// restart behaviour as Conn). The caller closes it; Close of the bridge does not.
func (b *Bridge) NewConn() (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///nurtuo-bridge",
		grpc.WithContextDialer(b.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCreds{token: b.token}),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
		grpc.WithConnectParams(grpc.ConnectParams{
			// Short reconnect backoff: the dialer already waits for the restarted child.
			Backoff:           backoff.Config{BaseDelay: 50 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 500 * time.Millisecond},
			MinConnectTimeout: 5 * time.Second,
		}),
	)
}

// Client returns the generated gRPC client.
func (b *Bridge) Client() connectorv1.ConnectorServiceClient {
	return connectorv1.NewConnectorServiceClient(b.cc)
}

// Done is closed when the bridge was closed or failed for good.
func (b *Bridge) Done() <-chan struct{} { return b.done }

// Err is the terminal failure (restart budget exhausted), or nil.
func (b *Bridge) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// Restarts is the number of successful restarts so far.
func (b *Bridge) Restarts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.restarts
}

// Address is the network and address the current child listens on ("" when none is running).
func (b *Bridge) Address() (network, address string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return "", ""
	}
	return b.cur.hs.Network, b.cur.hs.Address
}

// Implementation is the free-text implementation string from the handshake (e.g. "python/0.1.0").
func (b *Bridge) Implementation() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return ""
	}
	return b.cur.hs.Implementation
}

// Close stops supervision, closes the client connection and stops the child (gracefully, then
// by killing it). It is safe to call more than once.
func (b *Bridge) Close() error {
	b.mu.Lock()
	already := b.closed
	b.closed = true
	b.mu.Unlock()
	if already {
		return nil
	}
	b.cancel()
	b.markDone()
	if b.cc != nil {
		_ = b.cc.Close()
	}
	b.wg.Wait()
	b.mu.Lock()
	p := b.cur
	b.cur = nil
	b.mu.Unlock()
	if p != nil {
		p.stop(b.cfg.StopTimeout)
	}
	return nil
}

func (b *Bridge) markDone() { b.doneOnce.Do(func() { close(b.done) }) }

func (b *Bridge) fail(err error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.err = err
	b.mu.Unlock()
	b.log.Error("bridge failed", "err", err)
	b.markDone()
	_ = b.cc.Close() // RPCs now fail fast instead of waiting for a child that will not come back
}

func (b *Bridge) setCurrent(p *proc) {
	b.mu.Lock()
	b.cur = p
	close(b.ready)
	b.mu.Unlock()
}

// dial resolves to the current child; while it is restarting the dial waits for the new one.
func (b *Bridge) dial(ctx context.Context, _ string) (net.Conn, error) {
	for {
		b.mu.Lock()
		p, ready, closed, err := b.cur, b.ready, b.closed, b.err
		b.mu.Unlock()
		if closed {
			return nil, errClosed
		}
		if err != nil {
			return nil, err
		}
		if p != nil {
			var d net.Dialer
			return d.DialContext(ctx, p.hs.Network, p.hs.Address)
		}
		select {
		case <-ready:
		case <-b.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (b *Bridge) supervise(p *proc) {
	defer b.wg.Done()
	for {
		select {
		case <-p.exited:
		case <-b.ctx.Done():
			return
		}
		b.mu.Lock()
		b.cur = nil
		b.ready = make(chan struct{})
		b.mu.Unlock()
		p.cleanup()
		b.log.Warn("bridge process exited unexpectedly", "err", p.waitErr)

		np, err := b.restart()
		if err != nil {
			if !errors.Is(err, errClosed) {
				b.fail(err)
			}
			return
		}
		p = np
	}
}

// restart starts a replacement child, retrying within the restart budget. The budget
// (b.restartTimes) is owned by the supervisor goroutine and survives successful restarts.
func (b *Bridge) restart() (*proc, error) {
	times := &b.restartTimes
	for {
		now := time.Now()
		cut := now.Add(-b.cfg.RestartWindow)
		kept := (*times)[:0]
		for _, t := range *times {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		*times = kept
		if b.cfg.MaxRestarts < 0 || len(*times) >= b.cfg.MaxRestarts {
			return nil, fmt.Errorf("bridge: process exited unexpectedly and the restart budget (%d per %s) is exhausted",
				max(b.cfg.MaxRestarts, 0), b.cfg.RestartWindow)
		}
		delay := min(b.cfg.RestartBackoff<<len(*times), 5*time.Second)
		*times = append(*times, now)
		select {
		case <-time.After(delay):
		case <-b.ctx.Done():
			return nil, errClosed
		}
		ctx, cancel := context.WithTimeout(b.ctx, b.cfg.StartTimeout)
		p, err := b.launch(ctx)
		cancel()
		if b.ctx.Err() != nil {
			if p != nil {
				p.stop(b.cfg.StopTimeout)
			}
			return nil, errClosed
		}
		if err != nil {
			b.log.Warn("bridge restart failed", "err", err)
			continue
		}
		b.mu.Lock()
		b.restarts++
		b.mu.Unlock()
		b.setCurrent(p)
		b.log.Info("bridge restarted", "impl", p.hs.Implementation)
		return p, nil
	}
}

// proc is one child process.
type proc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	hs      Handshake
	exited  chan struct{} // closed after cmd.Wait returned
	waitErr error
	sockDir string
}

func (b *Bridge) launch(ctx context.Context) (*proc, error) {
	cfg := b.cfg
	listen := "tcp:127.0.0.1:0"
	var sockDir string
	if cfg.Network == "unix" {
		dir, err := os.MkdirTemp("", "nbr")
		if err != nil {
			return nil, fmt.Errorf("bridge: socket dir: %w", err)
		}
		sockDir = dir
		listen = "unix:" + filepath.Join(dir, "b.sock")
	} else if cfg.Network != "tcp" {
		return nil, fmt.Errorf("bridge: unknown network %q", cfg.Network)
	}

	first := make(chan string, 1)
	cmd := exec.Command(cfg.Command, cfg.Args...) //nolint:gosec // the command comes from the runtime configuration (bridge signatures are not verified yet), not from flow input
	cmd.Dir = cfg.Dir
	cmd.Env = childEnv(cfg, listen, b.token)
	cmd.Stderr = cfg.Stderr
	cmd.Stdout = &lineWriter{first: first, log: b.log}
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		removeDir(sockDir)
		return nil, fmt.Errorf("bridge: stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		removeDir(sockDir)
		return nil, fmt.Errorf("bridge: start %s: %w", cfg.Command, err)
	}
	p := &proc{cmd: cmd, stdin: stdin, exited: make(chan struct{}), sockDir: sockDir}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()

	timer := time.NewTimer(cfg.StartTimeout)
	defer timer.Stop()
	var line string
	select {
	case line = <-first:
	case <-p.exited:
		p.cleanup()
		return nil, fmt.Errorf("bridge: exited before the handshake: %v", p.waitErr)
	case <-timer.C:
		p.kill()
		return nil, fmt.Errorf("bridge: no handshake within %s", cfg.StartTimeout)
	case <-ctx.Done():
		p.kill()
		return nil, fmt.Errorf("bridge: waiting for handshake: %w", ctx.Err())
	}
	hs, err := ParseHandshake(line)
	if err == nil && !contains(cfg.Contracts, hs.Contract) {
		err = fmt.Errorf("bridge: bridge chose contract %q which was not offered (offered: %s)", hs.Contract, strings.Join(cfg.Contracts, ","))
	}
	if err != nil {
		p.kill()
		return nil, err
	}
	p.hs = hs
	return p, nil
}

// kill force-stops the child and waits for it.
func (p *proc) kill() {
	_ = p.stdin.Close()
	_ = p.cmd.Process.Kill()
	<-p.exited
	p.cleanup()
}

// stop closes stdin (graceful), waits grace, then kills.
func (p *proc) stop(grace time.Duration) {
	_ = p.stdin.Close()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-p.exited:
	case <-t.C:
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
	p.cleanup()
}

func (p *proc) cleanup() {
	_ = p.stdin.Close()
	removeDir(p.sockDir)
}

func removeDir(d string) {
	if d != "" {
		_ = os.RemoveAll(d)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// inheritedEnv are the only runtime environment variables a bridge receives.
var inheritedEnv = []string{
	"PATH", "PATHEXT", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR",
	"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "LANG", "LC_ALL", "TZ",
}

func childEnv(cfg Config, listen, token string) []string {
	var env []string
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, cfg.Env...)
	env = append(env,
		EnvListen+"="+listen,
		EnvContract+"="+strings.Join(cfg.Contracts, ","),
		EnvToken+"="+token,
		EnvWatchStdin+"=1",
	)
	return env
}

// lineWriter delivers the first stdout line (the handshake) and logs the rest.
type lineWriter struct {
	first chan<- string
	log   *slog.Logger
	buf   bytes.Buffer
	sent  bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			if w.buf.Len() > 64*1024 { // a bridge that never ends a line must not grow us unbounded
				w.buf.Reset()
			}
			return len(p), nil
		}
		line := string(w.buf.Next(i + 1))
		if !w.sent {
			w.sent = true
			w.first <- line
			continue
		}
		w.log.Debug("bridge stdout", "line", strings.TrimRight(line, "\r\n"))
	}
}

type tokenCreds struct{ token string }

func (c tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{TokenMetadataKey: c.token}, nil
}

func (tokenCreds) RequireTransportSecurity() bool { return false }
