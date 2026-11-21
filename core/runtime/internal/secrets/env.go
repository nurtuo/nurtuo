// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"context"
	"os"
	"strings"
)

// EnvPrefix is the environment variable prefix of the env provider.
const EnvPrefix = "NURTUO_SECRET_"

// Env reads secrets from NURTUO_SECRET_<NAME> (name upper-cased, every character outside
// [A-Z0-9] replaced by '_'). It is read-only.
type Env struct {
	// Lookup and Environ default to os.LookupEnv / os.Environ (test seams).
	Lookup  func(string) (string, bool)
	Environ func() []string
}

// NewEnv returns the env provider.
func NewEnv() *Env { return &Env{Lookup: os.LookupEnv, Environ: os.Environ} }

// EnvVar returns the environment variable that holds secret name.
func EnvVar(name string) string {
	var b strings.Builder
	b.WriteString(EnvPrefix)
	for _, c := range strings.ToUpper(name) {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (e *Env) Get(_ context.Context, name string) (Secret, error) {
	if err := ValidName(name); err != nil {
		return Secret{}, err
	}
	if v, ok := e.Lookup(EnvVar(name)); ok && v != "" {
		return New(v), nil
	}
	return Secret{}, notFound(name)
}

func (*Env) Set(context.Context, string, Secret) error { return ErrReadOnly }
func (*Env) Delete(context.Context, string) error      { return ErrReadOnly }

// List returns the (upper-case) names derivable from set environment variables.
func (e *Env) List(context.Context) ([]string, error) {
	var out []string
	for _, kv := range e.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, EnvPrefix) && len(k) > len(EnvPrefix) && v != "" {
			out = append(out, strings.TrimPrefix(k, EnvPrefix))
		}
	}
	return sorted(out), nil
}
