// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// Redactor replaces known secret values in strings with "***". It is safe for concurrent use.
type Redactor struct {
	mu   sync.RWMutex
	vals []string // longest first
}

// NewRedactor returns an empty redactor.
func NewRedactor() *Redactor { return &Redactor{} }

// Add registers values. Values shorter than 3 bytes are ignored (they would shred unrelated
// text); such secrets are still never logged by the code paths in this repo.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if len(v) < 3 {
			continue
		}
		dup := false
		for _, x := range r.vals {
			dup = dup || x == v
		}
		if !dup {
			r.vals = append(r.vals, v)
		}
	}
	sort.SliceStable(r.vals, func(i, j int) bool { return len(r.vals[i]) > len(r.vals[j]) })
}

// Redact scrubs every registered value from s.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.vals {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

// RedactError returns err with its message redacted (nil stays nil). The chain is dropped when the
// message changes, so wrapped errors cannot leak the original text through Unwrap.
func (r *Redactor) RedactError(err error) error {
	if err == nil || r == nil {
		return err
	}
	if m := err.Error(); r.Redact(m) != m {
		return redacted{r.Redact(m)}
	}
	return err
}

type redacted struct{ msg string }

func (e redacted) Error() string { return e.msg }

// Handler wraps a slog.Handler so messages and string/error attributes are redacted.
func (r *Redactor) Handler(next slog.Handler) slog.Handler { return &redactHandler{r: r, next: next} }

type redactHandler struct {
	r    *Redactor
	next slog.Handler
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool { out.AddAttrs(h.attr(a)); return true })
	return h.next.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	cp := make([]slog.Attr, len(as))
	for i, a := range as {
		cp[i] = h.attr(a)
	}
	return &redactHandler{r: h.r, next: h.next.WithAttrs(cp)}
}

func (h *redactHandler) WithGroup(n string) slog.Handler {
	return &redactHandler{r: h.r, next: h.next.WithGroup(n)}
}

func (h *redactHandler) attr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(h.r.Redact(a.Value.String()))
	case slog.KindGroup:
		g := a.Value.Group()
		ng := make([]slog.Attr, len(g))
		for i, x := range g {
			ng[i] = h.attr(x)
		}
		a.Value = slog.GroupValue(ng...)
	case slog.KindAny:
		if e, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(h.r.Redact(e.Error()))
		} else {
			a.Value = slog.StringValue(h.r.Redact(a.Value.String()))
		}
	default:
	}
	return a
}
