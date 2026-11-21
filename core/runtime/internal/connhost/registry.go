// SPDX-License-Identifier: AGPL-3.0-only

package connhost

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/connectors/nats"
	"github.com/nurtuo/nurtuo/connectors/postgres"
	"github.com/nurtuo/nurtuo/connectors/sftp"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
)

// Factory creates a fresh connector instance (one per connection, so pools are not shared).
type Factory func() connector.Connector

// Registry maps connector ids to factories.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Factory{}} }

// Register adds or replaces a connector factory.
func (r *Registry) Register(id string, f Factory) {
	r.mu.Lock()
	r.m[id] = f
	r.mu.Unlock()
}

// New creates an instance of connector id.
func (r *Registry) New(id string) (connector.Connector, error) {
	r.mu.RLock()
	f, ok := r.m[id]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("connector %q is not registered (available: %s)", id, strings.Join(r.IDs(), ", "))
	}
	return f(), nil
}

// IDs returns the registered connector ids, sorted.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for id := range r.m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Builtins returns a registry holding the native connectors shipped in connectors/: http, sftp,
// postgres and nats. Bridge connectors register through the same Register call from config.
func Builtins() *Registry {
	r := NewRegistry()
	r.Register("http", func() connector.Connector { return http.New() })
	r.Register("sftp", func() connector.Connector { return sftp.New() })
	r.Register("postgres", func() connector.Connector { return postgres.New() })
	r.Register("nats", func() connector.Connector { return nats.New() })
	return r
}
