// SPDX-License-Identifier: AGPL-3.0-only

package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"

	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
)

// stateDeployer is a fakeDeployer that also reports live state (StateProvider).
type stateDeployer struct {
	*fakeDeployer
	mu2    sync.Mutex
	states map[string][2]string
}

func newStateDeployer() *stateDeployer {
	return &stateDeployer{fakeDeployer: newFakeDeployer(), states: map[string][2]string{}}
}

func (s *stateDeployer) State(id string) (string, string) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	v := s.states[id]
	return v[0], v[1]
}

func (s *stateDeployer) set(id, state, msg string) {
	s.mu2.Lock()
	s.states[id] = [2]string{state, msg}
	s.mu2.Unlock()
}

func TestReportsPausedAndMessageFromStateProvider(t *testing.T) {
	sd := newStateDeployer()
	sd.set("a", StateRunning, "connections: erp ok")
	s := New(sd)
	var sts []Status
	rep := func(st Status) { sts = append(sts, st) }
	if err := s.Reconcile(context.Background(), 4, []Deployment{{ID: "a", BundleDigest: "x"}, {ID: "b", BundleDigest: "y"}}, rep); err != nil {
		t.Fatal(err)
	}
	// "a" carries the deployer's message; "b" (state unknown) defaults to running.
	var a, b Status
	for _, st := range sts {
		if st.State == StateRunning && st.DeploymentID == "a" {
			a = st
		}
		if st.State == StateRunning && st.DeploymentID == "b" {
			b = st
		}
	}
	if a.Message != "connections: erp ok" || b.DeploymentID != "b" {
		t.Fatalf("statuses = %+v", sts)
	}

	// "b" has no state yet: nothing is reported for it, and nothing changed for "a".
	sts = nil
	s.ReportChanges(context.Background(), rep)
	if len(sts) != 0 {
		t.Fatalf("unexpected reports %+v", sts)
	}

	// A command pauses "a": the supervisor reports paused once, with the last applied revision.
	sd.set("a", StatePaused, "paused listeners: a/flow")
	s.ReportChanges(context.Background(), rep)
	if len(sts) != 1 || sts[0].State != StatePaused || sts[0].DeploymentID != "a" || sts[0].Revision != 4 || sts[0].Message == "" {
		t.Fatalf("paused report = %+v", sts)
	}
	s.ReportChanges(context.Background(), rep)
	if len(sts) != 1 {
		t.Fatalf("paused reported twice: %+v", sts)
	}

	// Resume is reported as running again.
	sd.set("a", StateRunning, "")
	s.ReportChanges(context.Background(), rep)
	if len(sts) != 2 || sts[1].State != StateRunning {
		t.Fatalf("resume report = %+v", sts)
	}
}

func TestHandlerReportsAfterCommand(t *testing.T) {
	sd := newStateDeployer()
	s := New(sd)
	if err := s.Reconcile(context.Background(), 2, []Deployment{{ID: "a", BundleDigest: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	var got []*gatewayv1.DeploymentStatus
	h := &Handler{S: s, Report: func(st *gatewayv1.DeploymentStatus) { got = append(got, st) },
		Commands: map[string]CommandFunc{
			"pause_listener": func(context.Context, map[string]string) error { sd.set("a", StatePaused, "paused"); return nil },
			"broken":         func(context.Context, map[string]string) error { sd.set("a", "x", ""); return errors.New("no") },
		}}
	if r := h.HandleCommand(context.Background(), &gatewayv1.Command{CommandId: "1", Name: "broken"}); r.Ok || len(got) != 0 {
		t.Fatalf("failed command must not report: %v %v", r, got)
	}
	sd.set("a", StateRunning, "")
	if r := h.HandleCommand(context.Background(), &gatewayv1.Command{CommandId: "2", Name: "pause_listener"}); !r.Ok {
		t.Fatal(r.Message)
	}
	if len(got) != 1 || got[0].State != "paused" || got[0].DeploymentId != "a" || got[0].Revision != 2 {
		t.Fatalf("got %+v", got)
	}
}
