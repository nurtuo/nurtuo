// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Desired describes one listener that should exist. Key identifies it (e.g. "<flow>/<revision>");
// Spec is an opaque config fingerprint: a changed Spec restarts the listener. Factory builds the
// Listener and Handler on (re)start so the manager stays transport-agnostic.
type Desired struct {
	Key     string
	Spec    string
	Paused  bool
	Factory func() (Listener, Handler, error)
}

type entry struct {
	spec string
	l    Listener
}

// Manager reconciles running listeners with a desired-state list.
type Manager struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// NewManager returns an empty Manager.
func NewManager() *Manager { return &Manager{entries: map[string]*entry{}} }

// Apply starts new listeners, restarts changed ones, stops removed ones and aligns pause state. It
// continues past individual failures and returns them joined.
func (m *Manager) Apply(ctx context.Context, desired []Desired) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	want := map[string]bool{}
	for _, d := range desired {
		want[d.Key] = true
	}
	for k, e := range m.entries {
		if !want[k] {
			if err := e.l.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", k, err))
			}
			delete(m.entries, k)
		}
	}
	for _, d := range desired {
		e, ok := m.entries[d.Key]
		if ok && e.spec != d.Spec {
			if err := e.l.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", d.Key, err))
			}
			delete(m.entries, d.Key)
			ok = false
		}
		if !ok {
			l, h, err := d.Factory()
			if err == nil {
				err = l.Start(ctx, h)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("start %s: %w", d.Key, err))
				continue
			}
			e = &entry{spec: d.Spec, l: l}
			m.entries[d.Key] = e
		}
		var err error
		if d.Paused {
			err = e.l.Pause()
		} else {
			err = e.l.Resume()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pause/resume %s: %w", d.Key, err))
		}
	}
	return errors.Join(errs...)
}

// Status returns the status of every managed listener by key.
func (m *Manager) Status() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.entries))
	for k, e := range m.entries {
		out[k] = e.l.Status()
	}
	return out
}

// Get returns the listener for key.
func (m *Manager) Get(key string) (Listener, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, false
	}
	return e.l, true
}

// Shutdown stops all listeners.
func (m *Manager) Shutdown(ctx context.Context) error { return m.Apply(ctx, nil) }
