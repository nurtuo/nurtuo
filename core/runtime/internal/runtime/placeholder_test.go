// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
)

// TestPlaceholderReportsRunning characterizes the pre-fix behaviour (#58): the placeholder
// deployer accepts everything, so the supervisor reports running. Inverted by the fix.
func TestPlaceholderReportsRunning(t *testing.T) {
	var got []supervisor.Status
	s := supervisor.New(newMemDeployer())
	_ = s.Reconcile(context.Background(), 1, []supervisor.Deployment{{ID: "d1", BundleDigest: "sha256:aa"}},
		func(st supervisor.Status) { got = append(got, st) })
	if len(got) == 0 || got[len(got)-1].State != supervisor.StateRunning {
		t.Fatalf("statuses = %+v, want final running", got)
	}
}
