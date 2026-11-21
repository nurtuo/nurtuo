// SPDX-License-Identifier: AGPL-3.0-only

// Package maps implements profiles (JSON/XML field trees), the map compiler
// (JSONata field expressions, CEL conditions) and map execution over documents.
package maps

import (
	"context"
	"errors"
)

// ErrNoRuntime is returned by the default script runner.
var ErrNoRuntime = errors.New("maps: no script runtime configured")

// ScriptStep runs a user script over a document payload.
// The wazero-backed implementation is engine.ScriptStepFor; the flow `script` step uses engine.ScriptRunner.
type ScriptStep interface {
	Run(ctx context.Context, input []byte) ([]byte, error)
}

// UnavailableScript is a ScriptStep that always fails with ErrNoRuntime.
type UnavailableScript struct{}

// Run implements ScriptStep.
func (UnavailableScript) Run(context.Context, []byte) ([]byte, error) { return nil, ErrNoRuntime }
