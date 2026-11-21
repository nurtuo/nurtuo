// SPDX-License-Identifier: AGPL-3.0-only

package supervisor

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// fakeDeployer is an in-memory Deployer recording calls.
type fakeDeployer struct {
	mu      sync.Mutex
	running map[string]Deployment
	calls   []string
	failOn  map[string]error
}

func newFakeDeployer() *fakeDeployer {
	return &fakeDeployer{running: map[string]Deployment{}, failOn: map[string]error{}}
}

func (f *fakeDeployer) Actual(context.Context) (map[string]Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]Deployment{}
	for k, v := range f.running {
		out[k] = v
	}
	return out, nil
}

func (f *fakeDeployer) Apply(_ context.Context, d Deployment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[d.ID]; err != nil {
		return err
	}
	f.calls = append(f.calls, "apply:"+d.ID)
	f.running[d.ID] = d
	return nil
}

func (f *fakeDeployer) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "remove:"+id)
	delete(f.running, id)
	return nil
}

func TestReconcileStartUpdateStop(t *testing.T) {
	ctx := context.Background()
	fd := newFakeDeployer()
	fd.running["old"] = Deployment{ID: "old", BundleDigest: "x"}
	fd.running["same"] = Deployment{ID: "same", BundleDigest: "s", QueueIDs: []string{"a", "b"}}
	fd.running["upd"] = Deployment{ID: "upd", BundleDigest: "v1"}
	s := New(fd)
	var sts []Status
	err := s.Reconcile(ctx, 7, []Deployment{
		{ID: "same", BundleDigest: "s", QueueIDs: []string{"b", "a"}},
		{ID: "upd", BundleDigest: "v2"},
		{ID: "new", BundleDigest: "n"},
	}, func(st Status) { sts = append(sts, st) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"remove:old", "apply:upd", "apply:new"}; !slices.Equal(fd.calls, want) {
		t.Fatalf("calls = %v, want %v", fd.calls, want)
	}
	if s.Revision() != 7 {
		t.Fatalf("revision = %d", s.Revision())
	}
	if len(sts) != 4 || sts[3].State != StateRunning || sts[3].DeploymentID != "new" {
		t.Fatalf("statuses = %+v", sts)
	}
	fd.calls = nil
	err = s.Reconcile(ctx, 7, []Deployment{
		{ID: "same", BundleDigest: "s", QueueIDs: []string{"a", "b"}},
		{ID: "upd", BundleDigest: "v2"},
		{ID: "new", BundleDigest: "n"},
	}, nil)
	if err != nil || len(fd.calls) != 0 {
		t.Fatalf("expected no-op, err=%v calls=%v", err, fd.calls)
	}
}

func TestReconcileFailureKeepsRevision(t *testing.T) {
	fd := newFakeDeployer()
	fd.failOn["bad"] = errors.New("boom")
	s := New(fd)
	var sts []Status
	err := s.Reconcile(context.Background(), 3, []Deployment{{ID: "bad"}, {ID: "ok"}}, func(st Status) { sts = append(sts, st) })
	if err == nil {
		t.Fatal("expected error")
	}
	if s.Revision() != 0 {
		t.Fatalf("revision advanced to %d", s.Revision())
	}
	if _, ok := fd.running["ok"]; !ok {
		t.Fatal("healthy deployment should still be applied")
	}
	found := false
	for _, st := range sts {
		if st.DeploymentID == "bad" && st.State == StateError {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error status: %+v", sts)
	}
}
