// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/core/runtime/internal/runtime"
)

func init() { register("run", runMain) }

// nativeConnectors is the connector registry of this binary: the native connectors from
// connectors/ (http, sftp, postgres, nats). Bridge connectors will be added from config.
func nativeConnectors() *connhost.Registry { return connhost.Builtins() }

// runOptions are the runtime options every entry point (run, service run) uses.
func runOptions(logger *slog.Logger) runtime.Options {
	return runtime.Options{Logger: logger, Connectors: nativeConnectors()}
}

// runMain runs the runtime in the foreground until SIGINT/SIGTERM.
func runMain(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", defaultConfigPath(goos(), os.Getenv), "path to runtime config (YAML or JSON)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "Usage: nurtuo run [--config path]\n(to run a single flow file, use: nurtuo flow run <flow.json>)\n")
		return 2
	}
	cfg, err := runtime.LoadConfig(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo run: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if err := runtime.RunWith(ctx, *cfg, runOptions(logger)); err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo run: %v\n", err)
		return 1
	}
	return 0
}
