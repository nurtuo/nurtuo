// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"

	"github.com/nurtuo/nurtuo/core/runtime/internal/runtime"
)

const (
	serviceName        = "nurtuo"
	serviceDisplayName = "Nurtuo Runtime"
	serviceDescription = "Nurtuo integration runtime (outbound-only, managed by the control plane)"
	stopTimeout        = 20 * time.Second
)

func init() { register("service", serviceMain) }

// defaultConfigPath is the per-OS config location: ProgramData / /etc / /Library.
func defaultConfigPath(goos string, getenv func(string) string) string {
	switch goos {
	case "windows":
		pd := getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\Nurtuo\runtime.yaml`
	case "darwin":
		return "/Library/Application Support/Nurtuo/runtime.yaml"
	default:
		return "/etc/nurtuo/runtime.yaml"
	}
}

var serviceActions = []string{"install", "uninstall", "start", "stop", "status", "run"}

type serviceArgs struct {
	Action     string
	ConfigPath string
}

// parseServiceArgs parses `<action> [--config path]`.
func parseServiceArgs(args []string, defaultConfig string) (serviceArgs, error) {
	var sa serviceArgs
	if len(args) == 0 {
		return sa, fmt.Errorf("missing action (one of: %s)", strings.Join(serviceActions, ", "))
	}
	sa.Action = args[0]
	ok := false
	for _, a := range serviceActions {
		ok = ok || a == sa.Action
	}
	if !ok {
		return sa, fmt.Errorf("unknown action %q (one of: %s)", sa.Action, strings.Join(serviceActions, ", "))
	}
	fs := flag.NewFlagSet("service "+sa.Action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&sa.ConfigPath, "config", defaultConfig, "path to runtime config (YAML or JSON)")
	if err := fs.Parse(args[1:]); err != nil {
		return sa, err
	}
	if fs.NArg() > 0 {
		return sa, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return sa, nil
}

// program adapts the runtime to kardianos service.Interface. Start must not block.
type program struct {
	run func(ctx context.Context) error // blocks until ctx is cancelled
	log func(error)                     // reports a fatal run error
	// exit terminates the process with a non-zero code when run fails on its own (not because of
	// Stop). The service manager then sees a failed process and applies Restart=on-failure /
	// OnFailure=restart. Nil is a no-op (tests).
	exit func(code int)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(service.Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		err := p.run(ctx)
		if err == nil {
			return
		}
		if p.log != nil {
			p.log(err)
		}
		// A failure after Stop cancelled ctx is part of shutdown; Stop reports it.
		if ctx.Err() == nil && p.exit != nil {
			p.exit(1)
		}
	}(p.done)
	return nil
}

// Stop cancels the run context (graceful shutdown) and waits up to stopTimeout.
func (p *program) Stop(service.Service) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-time.After(stopTimeout):
		return errors.New("timed out waiting for runtime to stop")
	}
}

func serviceConfig(configPath string) *service.Config {
	return &service.Config{
		Name:        serviceName,
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		Arguments:   []string{"service", "run", "--config", configPath},
		Option: service.KeyValue{
			"Restart":                "on-failure", // systemd
			"KeepAlive":              true,         // launchd
			"RunAtLoad":              true,         // launchd
			"OnFailure":              "restart",    // windows recovery
			"OnFailureDelayDuration": "5s",
		},
	}
}

// slogWriter forwards slog text lines to the OS logger (Event Log / journald / syslog).
type slogWriter struct{ l service.Logger }

func (w slogWriter) Write(b []byte) (int, error) {
	line := strings.TrimRight(string(b), "\r\n")
	switch {
	case strings.Contains(line, "level=ERROR"):
		_ = w.l.Error(line)
	case strings.Contains(line, "level=WARN"):
		_ = w.l.Warning(line)
	default:
		_ = w.l.Info(line)
	}
	return len(b), nil
}

func serviceMain(args []string, stdout, stderr io.Writer) int {
	sa, err := parseServiceArgs(args, defaultConfigPath(goos(), os.Getenv))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo service: %v\n", err)
		return 2
	}
	if err := runServiceAction(sa, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo service %s: %v\n", sa.Action, err)
		return 1
	}
	return 0
}

func runServiceAction(sa serviceArgs, stdout io.Writer) error {
	cfgPath, err := filepath.Abs(sa.ConfigPath)
	if err != nil {
		return err
	}
	prg := &program{}
	svc, err := service.New(prg, serviceConfig(cfgPath))
	if err != nil {
		return err
	}
	switch sa.Action {
	case "install":
		cfg, err := runtime.LoadConfig(cfgPath)
		if err != nil {
			return fmt.Errorf("refusing to install with invalid config: %w", err)
		}
		// Installed services always get a state_dir: queued messages and the revision survive restarts.
		sd := defaultServiceStateDir(goos(), os.Getenv)
		if added, err := ensureStateDirConfigured(cfgPath, sd, cfg.StateDir != ""); err != nil {
			return err
		} else if added {
			_, _ = fmt.Fprintf(stdout, "state_dir not set in %s: added %s\n", cfgPath, sd)
		}
		if err := svc.Install(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "service %q installed (config %s)\n", serviceName, cfgPath)
	case "uninstall", "start", "stop":
		if err := service.Control(svc, sa.Action); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "service %q: %s ok\n", serviceName, sa.Action)
	case "status":
		st, err := svc.Status()
		if errors.Is(err, service.ErrNotInstalled) {
			_, _ = fmt.Fprintln(stdout, "not installed")
			return nil
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, statusString(st))
	case "run":
		return runService(svc, prg, cfgPath)
	}
	return nil
}

func statusString(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	}
	return "unknown"
}

// runService is the entry point the OS service manager invokes (also works interactively: Ctrl+C stops).
func runService(svc service.Service, prg *program, cfgPath string) error {
	cfg, err := runtime.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	var logger *slog.Logger
	if sl, lerr := svc.Logger(nil); lerr == nil && !service.Interactive() {
		logger = slog.New(slog.NewTextHandler(slogWriter{sl}, nil))
	} else {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	prg.run = func(ctx context.Context) error {
		return runtime.RunWith(ctx, *cfg, runOptions(logger))
	}
	prg.log = func(err error) { logger.Error("runtime failed", "err", err) }
	prg.exit = os.Exit
	return svc.Run()
}
