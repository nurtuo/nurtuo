// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
)

// Process holds process-scoped properties shared by every document of one execution.
type Process struct {
	mu    sync.RWMutex
	props map[string]string
}

// NewProcess returns a Process with a copy of the initial properties.
func NewProcess(initial map[string]string) *Process {
	p := &Process{props: map[string]string{}}
	for k, v := range initial {
		p.props[k] = v
	}
	return p
}

func (p *Process) Get(k string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	v, ok := p.props[k]
	return v, ok
}

func (p *Process) Set(k, v string) {
	p.mu.Lock()
	p.props[k] = v
	p.mu.Unlock()
}

// Props returns a copy of the process properties.
func (p *Process) Props() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]string, len(p.props))
	for k, v := range p.props {
		out[k] = v
	}
	return out
}

// Evaluator evaluates Decision/Route conditions (CEL) and Message/SetProperties templates. The
// production implementation (cel-go) plugs in here; BasicEvaluator is a dependency-free subset.
type Evaluator interface {
	Condition(expr string, d *docs.Document, p *Process) (bool, error)
	Template(tmpl string, d *docs.Document, p *Process) (string, error)
}

// BasicEvaluator supports: true/false, `<operand> <op> <operand>` with op in == != < <= > >=, and
// `!<expr>`. Operands: 'str' or "str" literals, numbers, doc.id, doc.props.NAME, process.props.NAME
// (missing properties are the empty string). Templates substitute {{operand}}.
type BasicEvaluator struct{ templateBytes *atomic.Int64 }

// NewBoundedEvaluator limits the total bytes expanded by templates in one execution.
func NewBoundedEvaluator(maxBytes int64) BasicEvaluator {
	budget := new(atomic.Int64)
	budget.Store(maxBytes)
	return BasicEvaluator{templateBytes: budget}
}

func (BasicEvaluator) Condition(expr string, d *docs.Document, p *Process) (bool, error) {
	e := strings.TrimSpace(expr)
	switch e {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if strings.HasPrefix(e, "!") && !strings.HasPrefix(e, "!=") {
		v, err := BasicEvaluator{}.Condition(e[1:], d, p)
		return !v, err
	}
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if i := indexOutsideQuotes(e, op); i >= 0 {
			l, err := operand(strings.TrimSpace(e[:i]), d, p)
			if err != nil {
				return false, err
			}
			r, err := operand(strings.TrimSpace(e[i+len(op):]), d, p)
			if err != nil {
				return false, err
			}
			return compare(l, r, op), nil
		}
	}
	v, err := operand(e, d, p)
	if err != nil {
		return false, err
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("condition %q is not boolean", expr)
	}
	return b, nil
}

func (e BasicEvaluator) Template(t string, d *docs.Document, p *Process) (string, error) {
	var sb strings.Builder
	appendText := func(text string) error {
		if budget := e.templateBytes; budget != nil {
			size := int64(len(text))
			for {
				remaining := budget.Load()
				if size > remaining {
					return ErrReturnLimit
				}
				if budget.CompareAndSwap(remaining, remaining-size) {
					break
				}
			}
		}
		sb.WriteString(text)
		return nil
	}
	for {
		i := strings.Index(t, "{{")
		if i < 0 {
			if err := appendText(t); err != nil {
				return "", err
			}
			return sb.String(), nil
		}
		j := strings.Index(t[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("template: unterminated {{ in %q", t)
		}
		if err := appendText(t[:i]); err != nil {
			return "", err
		}
		v, err := operand(strings.TrimSpace(t[i+2:i+j]), d, p)
		if err != nil {
			return "", err
		}
		if err := appendText(v); err != nil {
			return "", err
		}
		t = t[i+j+2:]
	}
}

func indexOutsideQuotes(s, op string) int {
	var q byte
	for i := 0; i+len(op) <= len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
		case s[i:i+len(op)] == op:
			return i
		}
	}
	return -1
}

func operand(s string, d *docs.Document, p *Process) (string, error) {
	switch {
	case len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0]:
		return s[1 : len(s)-1], nil
	case s == "doc.id":
		return d.ID, nil
	case strings.HasPrefix(s, "doc.props."):
		return d.Props[s[len("doc.props."):]], nil
	case strings.HasPrefix(s, "process.props."):
		v, _ := p.Get(s[len("process.props."):])
		return v, nil
	case s == "true" || s == "false":
		return s, nil
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s, nil
	}
	return "", fmt.Errorf("unsupported expression %q", s)
}

func compare(l, r, op string) bool {
	lf, e1 := strconv.ParseFloat(l, 64)
	rf, e2 := strconv.ParseFloat(r, 64)
	var c int
	if e1 == nil && e2 == nil {
		switch {
		case lf < rf:
			c = -1
		case lf > rf:
			c = 1
		}
	} else {
		c = strings.Compare(l, r)
	}
	switch op {
	case "==":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	default:
		return c >= 0
	}
}
