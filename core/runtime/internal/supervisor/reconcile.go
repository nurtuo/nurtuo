// SPDX-License-Identifier: AGPL-3.0-only

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
)

// Deployment states reported upstream (open set per gateway.proto).
const (
	StateApplying = "applying"
	StateRunning  = "running"
	StateError    = "error"
	StatePaused   = "paused"
)

// StateProvider is optionally implemented by a Deployer that knows more than "applied": the live
// state of a deployment (running or paused) and a status message (for example the connection test
// summary). An empty state means "unknown, assume running".
type StateProvider interface {
	State(id string) (state, message string)
}

// Status is one deployment status transition, reported to the control plane.
type Status struct {
	Revision     uint64
	DeploymentID string
	State        string
	Message      string
}

// Reporter receives status transitions. It must not block for long.
type Reporter func(Status)

// Supervisor reconciles desired state against the Deployer.
type Supervisor struct {
	d Deployer

	applyMu    sync.Mutex // serialises Reconcile; held for the whole (possibly slow) apply
	mu         sync.Mutex // guards revision, reconciled and last; held only briefly, never across deployer calls
	revision   uint64
	reconciled bool
	revPath    string            // when set, the applied revision is persisted here
	last       map[string]string // deployment -> last reported state
}

func New(d Deployer) *Supervisor { return &Supervisor{d: d, last: map[string]string{}} }

func (s *Supervisor) liveState(id string) (string, string) {
	if sp, ok := s.d.(StateProvider); ok {
		if st, msg := sp.State(id); st != "" {
			return st, msg
		}
	}
	return StateRunning, ""
}

// ReportChanges reports every deployment whose live state differs from the last reported one (for
// example running -> paused after a pause_listener command). It uses the last applied revision.
func (s *Supervisor) ReportChanges(ctx context.Context, report Reporter) {
	sp, ok := s.d.(StateProvider)
	if !ok || report == nil {
		return
	}
	actual, err := s.d.Actual(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(actual))
	for id := range actual {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st, msg := sp.State(id)
		if st == "" || st == s.last[id] {
			continue
		}
		s.last[id] = st
		report(Status{s.revision, id, st, msg})
	}
}

// Revision is the last fully applied desired-state revision.
func (s *Supervisor) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// liveRevision requests a replay until this process has applied desired state.
func (s *Supervisor) liveRevision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reconciled {
		return 0
	}
	return s.revision
}

// Reconcile drives actual state to the desired set. Per-deployment failures are reported and
// joined into the returned error; the revision only advances when everything succeeded, so the
// control plane resends on the next handshake. Reconciles are serialised.
func (s *Supervisor) Reconcile(ctx context.Context, revision uint64, desired []Deployment, report Reporter) error {
	if report == nil {
		report = func(Status) {}
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	actual, err := s.d.Actual(ctx)
	if err != nil {
		return fmt.Errorf("list actual deployments: %w", err)
	}
	want := make(map[string]Deployment, len(desired))
	for _, d := range desired {
		want[d.ID] = d
	}

	var errs []error
	var stale []string
	for id := range actual {
		if _, ok := want[id]; !ok {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		if err := s.d.Remove(ctx, id); err != nil {
			report(Status{revision, id, StateError, err.Error()})
			errs = append(errs, fmt.Errorf("remove %s: %w", id, err))
		}
	}
	for _, d := range desired {
		cur, exists := actual[d.ID]
		if exists && equal(cur, d) {
			continue
		}
		report(Status{revision, d.ID, StateApplying, ""})
		if err := s.d.Apply(ctx, d); err != nil {
			report(Status{revision, d.ID, StateError, err.Error()})
			errs = append(errs, fmt.Errorf("apply %s: %w", d.ID, err))
			continue
		}
		state, msg := s.liveState(d.ID)
		s.mu.Lock()
		s.last[d.ID] = state
		s.mu.Unlock()
		report(Status{revision, d.ID, state, msg})
	}
	s.mu.Lock()
	for _, id := range stale {
		delete(s.last, id)
	}
	s.mu.Unlock()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if s.revPath != "" {
		if err := saveRevision(s.revPath, revision); err != nil {
			// Leave the advertised revision unchanged so the control plane retries.
			return fmt.Errorf("persist revision %d: %w", revision, err)
		}
	}
	s.mu.Lock()
	s.reconciled = true
	s.revision = revision
	s.mu.Unlock()
	return nil
}

func equal(a, b Deployment) bool {
	return a.BundleDigest == b.BundleDigest &&
		sameSet(a.QueueIDs, b.QueueIDs) && sameSet(a.ListenerIDs, b.ListenerIDs)
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
