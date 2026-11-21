// SPDX-License-Identifier: AGPL-3.0-only

// Package secrets holds the runtime's secret providers (env, dir, encrypted-file), the opaque
// Secret value type and the Redactor that scrubs secret values from strings and logs.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
)

// ErrNotFound is returned (wrapped, with the secret name) for a missing secret.
var ErrNotFound = errors.New("secret not found")

// ErrReadOnly is returned by Set/Delete on providers that cannot write (env).
var ErrReadOnly = errors.New("secret provider is read-only")

// Ref is the scheme prefix of a secret reference in connection settings.
const Ref = "secret://"

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ValidName reports whether name is a legal secret name. Names never contain path separators.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid secret name %q (allowed: letters, digits, '_', '.', '-'; max 128)", name)
	}
	return nil
}

// Secret is an opaque secret value. Every formatting path prints "***"; only Reveal returns the value.
type Secret struct{ v string }

// New wraps a value.
func New(v string) Secret { return Secret{v: v} }

// Reveal returns the plain value. Call it only where the value is handed to a connector.
func (s Secret) Reveal() string { return s.v }

// String implements fmt.Stringer.
func (Secret) String() string { return "***" }

// GoString implements fmt.GoStringer (%#v).
func (Secret) GoString() string { return "secrets.Secret(***)" }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue("***") }

// MarshalJSON never emits the value.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"***"`), nil }

// MarshalText never emits the value.
func (Secret) MarshalText() ([]byte, error) { return []byte("***"), nil }

// Provider resolves secrets by name.
type Provider interface {
	// Get returns the secret or an error wrapping ErrNotFound. Errors never contain the value.
	Get(ctx context.Context, name string) (Secret, error)
}

// Store is a Provider that can also be written (used by `nurtuo secret`).
type Store interface {
	Provider
	Set(ctx context.Context, name string, value Secret) error
	Delete(ctx context.Context, name string) error
	// List returns the secret names, sorted. Values are never listed.
	List(ctx context.Context) ([]string, error)
}

func notFound(name string) error { return fmt.Errorf("secret %q: %w", name, ErrNotFound) }

func sorted(s []string) []string { sort.Strings(s); return s }
