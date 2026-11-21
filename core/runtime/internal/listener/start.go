// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"context"
	"fmt"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// Start trigger types that are bound to a listener (others, e.g. manual/cron, run directly).
const (
	StartListener = "listener" // connector "http" (or empty) = HTTP listener; others are bridge listeners
	StartQueue    = "queue"
)

// IsListenerStart reports whether a Start config is bound to a push listener.
func IsListenerStart(c flow.StartConfig) bool { return c.Type == StartListener || c.Type == StartQueue }

// StartRunner is the engine StepRunner for the Start step. Delivered documents already carry the
// trigger metadata (set by the listener), so it validates the binding and passes the set through.
func StartRunner() engine.StepRunner {
	return engine.StepRunnerFunc(func(_ context.Context, sc *engine.StepContext, in *docs.Set) (*docs.Set, error) {
		cfg, ok := sc.Config.(flow.StartConfig)
		if !ok {
			return in, fmt.Errorf("start: unexpected config %T", sc.Config)
		}
		switch cfg.Type {
		case StartListener:
			if (cfg.Connector == "" || cfg.Connector == "http") && cfg.Path == "" {
				return in, fmt.Errorf("start: http listener requires a path")
			}
		case StartQueue:
			if cfg.Queue == "" {
				return in, fmt.Errorf("start: queue listener requires a queue")
			}
		}
		return in, nil
	})
}

// Register installs the start runner on reg so flows with listener-bound Start steps run.
func Register(reg *engine.Registry) { reg.Register(flow.KindStart, StartRunner()) }
