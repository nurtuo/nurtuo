// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Service is a long-running component started after the database.
type Service interface {
	// Run blocks until ctx is cancelled and the service has fully stopped.
	Run(ctx context.Context) error
	// WaitHealthy blocks until the service is serving or fails.
	WaitHealthy(ctx context.Context) error
}

// RuntimeFunc runs the local runtime against the control plane until ctx is cancelled.
type RuntimeFunc func(ctx context.Context, env RuntimeEnv) error

// RuntimeEnv is what the runtime needs to connect to the local control plane.
type RuntimeEnv struct {
	ControlPlaneURL string
	Creds           *Credentials
	Dirs            DataDirs
}

// Stack orders startup (bootstrap, DB, control plane, runtime registration, runtime) and reverse shutdown
// (runtime, control plane, DB).
type Stack struct {
	Dirs         DataDirs
	Bootstrap    Bootstrapper
	DB           Database
	ControlPlane func(dsn string, creds *Credentials) Service
	// Register, when set, runs once the control plane is healthy and before the runtime starts,
	// so the gateway knows the runtime when it says Hello. It must be idempotent across restarts.
	Register   func(ctx context.Context, env RuntimeEnv) error
	Runtime    RuntimeFunc
	CPURL      string
	Log        io.Writer
	HealthWait time.Duration
}

// Run blocks until ctx is cancelled or a component fails, then shuts down in order.
func (s *Stack) Run(ctx context.Context) (err error) {
	if err := s.Dirs.Ensure(); err != nil {
		return err
	}
	creds, err := s.Bootstrap.Bootstrap(s.Dirs)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := s.DB.Start(ctx); err != nil {
		return err
	}
	defer func() {
		// Stop DB last, with its own context so cancellation does not abort it.
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = errors.Join(err, s.DB.Stop(sctx))
	}()

	cp := s.ControlPlane(s.DB.DSN(), creds)
	cpCtx, cpCancel := context.WithCancel(context.Background())
	cpDone := make(chan error, 1)
	go func() { cpDone <- cp.Run(cpCtx) }()
	// Control plane is stopped (and awaited) before the deferred DB stop runs.
	defer func() { cpCancel(); err = errors.Join(err, <-cpDone) }()

	wait := s.HealthWait
	if wait == 0 {
		wait = 60 * time.Second
	}
	hctx, hcancel := context.WithTimeout(ctx, wait)
	herr := cp.WaitHealthy(hctx)
	hcancel()
	if herr != nil {
		return fmt.Errorf("control plane: %w", herr)
	}
	env := RuntimeEnv{ControlPlaneURL: s.CPURL, Creds: creds, Dirs: s.Dirs}
	if s.Register != nil {
		if err := s.Register(ctx, env); err != nil {
			return fmt.Errorf("register runtime: %w", err)
		}
	}

	rtCtx, rtCancel := context.WithCancel(ctx)
	defer rtCancel()
	rtDone := make(chan error, 1)
	go func() { rtDone <- s.Runtime(rtCtx, env) }()
	select {
	case err = <-rtDone: // runtime exited on its own
	case err = <-cpDone: // control plane gave up (restart budget exhausted)
		cpDone <- err // re-queue for the deferred join
		if err == nil {
			err = errors.New("control plane exited")
		}
		rtCancel()
		if rerr := <-rtDone; rerr != nil {
			err = errors.Join(err, rerr) // joined; secondary to the control-plane failure
		}
	case <-ctx.Done():
		rtCancel()
		err = <-rtDone
	}
	rtCancel()
	return err
}

// controlPlaneService adapts Child + health probe.
type controlPlaneService struct {
	child *Child
	probe func(context.Context) error
}

func (c controlPlaneService) Run(ctx context.Context) error { return c.child.Run(ctx) }
func (c controlPlaneService) WaitHealthy(ctx context.Context) error {
	return WaitHealthy(ctx, c.probe, 250*time.Millisecond, time.Hour)
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

// runStandalone is the `nurtuo standalone` entry point (registered in standalone_register.go):
// runStandalone(os.Args[2:]) returns the exit code.
func runStandalone(args []string) int {
	fs := flag.NewFlagSet("standalone", flag.ContinueOnError)
	root, _ := defaultDataRoot()
	dataDir := fs.String("data-dir", root, "data directory")
	port := fs.Int("port", 8080, "control plane HTTP port")
	cpCmd := fs.String("control-plane-cmd", "node", "control plane executable")
	cpEntry := fs.String("control-plane-entry", defaultControlPlaneEntry, "control plane script passed to the executable; relative paths are searched from the executable and the working directory upwards (empty = none)")
	gatewayAddr := fs.String("gateway-addr", "127.0.0.1:8443", "control plane runtime gateway host:port (gRPC, mTLS)")
	dev := fs.Bool("dev", false, "development mode (reserved)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = dev
	*cpEntry = resolveControlPlaneEntry(*cpEntry, executableDir(), mustGetwd())
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "standalone: no data directory; use --data-dir or NURTUO_DATA_DIR")
		return 2
	}
	dirs := NewDataDirs(*dataDir)
	if err := ensureDataDirs(dirs, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "standalone:", err)
		return 1
	}
	logf, err := os.OpenFile(filepath.Join(dirs.Logs, "control-plane.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "standalone:", err)
		return 1
	}
	defer func() { _ = logf.Close() }()
	logw := &syncWriter{w: io.MultiWriter(logf, os.Stderr)}

	url := "http://127.0.0.1:" + strconv.Itoa(*port) + "/"
	creds, err := NewLocalBootstrapper().Bootstrap(dirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "standalone: bootstrap:", err)
		return 1
	}
	stack := &Stack{
		Dirs:      dirs,
		Bootstrap: NewLocalBootstrapper(),
		DB:        NewEmbeddedDatabase(dirs, 0, creds.DBPassword),
		CPURL:     url,
		Log:       logw,
		ControlPlane: func(dsn string, cr *Credentials) Service {
			argv := []string{*cpCmd}
			if *cpEntry != "" {
				argv = append(argv, *cpEntry)
			}
			return controlPlaneService{
				child: NewChild(ChildSpec{
					Name: "control-plane", Command: argv, Log: logw, MaxRestarts: 10,
					Env: controlPlaneEnv(dsn, cr, *port, *gatewayAddr),
				}),
				probe: HTTPProbe(url),
			}
		},
		Register: registerRealRuntime,
		Runtime:  runRealRuntime(*gatewayAddr),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := stack.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "standalone:", err)
		return 1
	}
	return 0
}

// controlPlaneEnv is the environment of the supervised control plane child: the database, the
// REST port, the mTLS runtime gateway (server certificate, client CA, port from gatewayAddr) and
// authentication. Auth is mandatory in the control plane: the generated admin token is the HMAC
// secret and the password of the bootstrap owner admin@nurtuo.local (read it from
// <data-dir>/secrets/admin-token).
func controlPlaneEnv(dsn string, c *Credentials, port int, gatewayAddr string) []string {
	env := []string{"DATABASE_URL=" + dsn, "PORT=" + strconv.Itoa(port)}
	if c == nil {
		return env
	}
	if _, gp, err := net.SplitHostPort(gatewayAddr); err == nil && gp != "" {
		env = append(env, "GATEWAY_PORT="+gp)
	}
	env = append(env,
		"GATEWAY_CERT="+c.ServerCertFile, "GATEWAY_KEY="+c.ServerKeyFile, "GATEWAY_CA="+c.CAFile,
		"NURTUO_BUNDLE_SIGNING_KEY_FILE="+c.BundleSigningKeyFile,
		"NURTUO_AUTH_SECRET="+c.AdminToken,
		"NURTUO_BOOTSTRAP_EMAIL="+standaloneAdminEmail, "NURTUO_BOOTSTRAP_PASSWORD="+c.AdminToken,
	)
	return env
}
