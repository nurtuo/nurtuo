// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// ErrNoRunner is returned when a flow uses a step kind with no registered StepRunner.
var ErrNoRunner = errors.New("engine: no runner registered for step kind")

// StepContext is what a StepRunner sees for one step execution.
type StepContext struct {
	Flow                    *flow.Flow
	Step                    flow.Step
	Config                  any // decoded config (flow.MessageConfig, flow.ConnectorConfig, ...)
	Process                 *Process
	Eval                    Evaluator
	Spool                   *docs.Spool
	maxConnectorOutputBytes int64
}

// StepRunner executes a single-output ("default" port) step over a whole document set. It returns
// the resulting set; documents may be mutated, replaced, dropped or failed (see docs.Apply).
// Connectors, queues, maps and notify steps plug in through this interface.
type StepRunner interface {
	Run(ctx context.Context, sc *StepContext, in *docs.Set) (*docs.Set, error)
}

// StepRunnerFunc adapts a function to StepRunner.
type StepRunnerFunc func(ctx context.Context, sc *StepContext, in *docs.Set) (*docs.Set, error)

func (f StepRunnerFunc) Run(ctx context.Context, sc *StepContext, in *docs.Set) (*docs.Set, error) {
	return f(ctx, sc, in)
}

// DocFunc is a per-document runner body (see PerDocument).
type DocFunc func(ctx context.Context, sc *StepContext, d *docs.Document) (*docs.Document, error)

// PerDocument lifts a per-document function to a StepRunner using docs.Apply.
func PerDocument(fn DocFunc) StepRunner {
	return StepRunnerFunc(func(ctx context.Context, sc *StepContext, in *docs.Set) (*docs.Set, error) {
		return docs.Apply(ctx, in, docs.Options{Step: sc.Step.ID}, func(ctx context.Context, d *docs.Document) (*docs.Document, error) {
			return fn(ctx, sc, d)
		})
	})
}

// Registry maps step kinds to runners.
type Registry struct{ m map[string]StepRunner }

// NewRegistry returns a registry with the built-in Message, SetProperties and Map runners. Call
// RegisterConnectorRunner to enable connector steps.
func NewRegistry() *Registry {
	r := &Registry{m: map[string]StepRunner{}}
	r.Register(flow.KindMessage, PerDocument(runMessage))
	r.Register(flow.KindSetProperties, PerDocument(runSetProperties))
	r.RegisterMapRunner()
	return r
}

// Register sets (or replaces) the runner for a step kind.
func (r *Registry) Register(kind string, sr StepRunner) { r.m[kind] = sr }

// Has reports whether a runner is registered for kind.
func (r *Registry) Has(kind string) bool { _, ok := r.m[kind]; return ok }

func (r *Registry) lookup(kind string) (StepRunner, error) {
	if sr, ok := r.m[kind]; ok {
		return sr, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNoRunner, kind)
}

func runMessage(_ context.Context, sc *StepContext, d *docs.Document) (*docs.Document, error) {
	cfg := sc.Config.(flow.MessageConfig)
	out, err := sc.Eval.Template(cfg.Template, d, sc.Process)
	if err != nil {
		return nil, err
	}
	p, err := sc.Spool.NewPayload(strings.NewReader(out))
	if err != nil {
		return nil, err
	}
	d.SetPayload(p)
	return d, nil
}

func runSetProperties(_ context.Context, sc *StepContext, d *docs.Document) (*docs.Document, error) {
	cfg := sc.Config.(flow.SetPropertiesConfig)
	for _, pa := range cfg.Properties {
		v, err := sc.Eval.Template(pa.Value, d, sc.Process)
		if err != nil {
			return nil, err
		}
		if pa.Scope == "process" {
			sc.Process.Set(pa.Name, v)
		} else {
			d.Props[pa.Name] = v
		}
	}
	return d, nil
}
