// SPDX-License-Identifier: Apache-2.0

package flow_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// TestReferenceExamples keeps the flows embedded in docs/flow-reference.md (one per step kind,
// examples/flows/reference) valid for the Go validator with no errors and no warnings, and
// checks that every step kind has an example that actually contains that kind.
func TestReferenceExamples(t *testing.T) {
	kinds := []string{
		flow.KindStart, flow.KindConnector, flow.KindQueue, flow.KindMap, flow.KindMessage,
		flow.KindSetProperties, flow.KindDecision, flow.KindBranch, flow.KindRoute, flow.KindTryCatch,
		flow.KindProcessCall, flow.KindNotify, flow.KindStop, flow.KindReturnDocuments,
	}
	for _, k := range kinds {
		raw, err := os.ReadFile(filepath.Join(examplesDir, "reference", k+".flow.json"))
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		f, diags, err := flow.Parse(raw)
		if err != nil || len(diags) != 0 {
			t.Fatalf("%s: err=%v diags=%+v", k, err, diags)
		}
		found := false
		for _, s := range f.Steps {
			if s.Kind == k {
				found = true
				if _, err := s.TypedConfig(); err != nil {
					t.Errorf("%s: TypedConfig: %v", k, err)
				}
			}
		}
		if !found {
			t.Errorf("example %s has no %s step", k, k)
		}
	}
}
