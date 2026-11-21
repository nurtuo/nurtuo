// SPDX-License-Identifier: AGPL-3.0-only

package bench

import (
	"context"
	"fmt"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

const overheadFlow = `{"schema":"nurtuo.flow/v1","id":"bench-overhead","name":"Bench","revision":1,
"steps":[{"id":"start","kind":"start","config":{"type":"manual"}},
{"id":"set","kind":"setProperties","config":{"properties":[{"name":"k","value":"v-{{doc.id}}"}]}},
{"id":"ret","kind":"returnDocuments","config":{}}],
"edges":{"start:default":"set","set:default":"ret"}}`

// Result is one scenario's outcome.
type Result struct {
	Scenario string  `json:"scenario"`
	Load     Load    `json:"load"`
	Errors   int64   `json:"errors"`
	Latency  Summary `json:"latency"`
}

// Load echoes the LoadConfig in JSON.
type Load struct {
	N           int `json:"n"`
	Concurrency int `json:"concurrency"`
	Rate        int `json:"rate"`
}

func loadOf(lc LoadConfig) Load { return Load(lc) }

// RunEngineOverhead runs Start -> SetProperties -> Return through engine.Engine, one document per
// run, and records the latency of each run. There is no external I/O, so the number is pure
// platform overhead.
func RunEngineOverhead(ctx context.Context, lc LoadConfig) (Result, error) {
	f, diags, err := flow.Parse([]byte(overheadFlow))
	if err != nil {
		return Result{}, fmt.Errorf("bench flow invalid: %w %v", err, diags)
	}
	eng := engine.New()
	h := NewHistogram()
	for i := 0; i < 50; i++ { // warm-up
		s := docs.NewSet()
		s.Add(docs.New("w", nil, nil))
		if _, err := eng.Run(ctx, f, s, nil); err != nil {
			return Result{}, err
		}
	}
	errs := drive(ctx, lc, h, func(ctx context.Context, i int) error {
		s := docs.NewSet()
		s.Add(docs.New(fmt.Sprintf("d%d", i), map[string]string{"a": "b"}, nil))
		res, err := eng.Run(ctx, f, s, nil)
		if err != nil {
			return err
		}
		if res.Returned.Len() != 1 {
			return fmt.Errorf("returned %d docs", res.Returned.Len())
		}
		return nil
	})
	return Result{Scenario: "engine_overhead", Load: loadOf(lc), Errors: errs, Latency: h.Summary()}, nil
}
