// SPDX-License-Identifier: AGPL-3.0-only

package maps

import (
	"errors"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"github.com/blues/jsonata-go"
)

// Mapping assigns the result of a JSONata expression evaluated against the source
// value to the target path (dot separated). When is an optional CEL condition over
// the variable `src`; the mapping is skipped when it is false.
type Mapping struct {
	Target string
	Expr   string
	When   string
}

// Definition is an uncompiled map.
type Definition struct {
	Name     string
	Source   *Profile
	Target   *Profile
	Mappings []Mapping
}

type compiledMapping struct {
	path   []string
	expr   *jsonata.Expr
	when   cel.Program
	target *Field
}

// Map is a compiled map, safe for concurrent Execute calls.
type Map struct {
	def      Definition
	mappings []compiledMapping
}

// Compile validates and compiles a definition. All errors are collected.
func Compile(def Definition) (*Map, error) {
	var errs []error
	if err := def.Source.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("source: %w", err))
	}
	if err := def.Target.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("target: %w", err))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	env, err := cel.NewEnv(cel.Variable("src", cel.DynType))
	if err != nil {
		return nil, err
	}
	m := &Map{def: def}
	for i, mp := range def.Mappings {
		cm := compiledMapping{path: strings.Split(mp.Target, ".")}
		f := def.Target.Root
		for _, seg := range cm.path {
			if f != nil {
				f = f.child(seg)
			}
		}
		if mp.Target == "" || f == nil {
			errs = append(errs, fmt.Errorf("mapping %d: target %q not in target profile", i, mp.Target))
			continue
		}
		cm.target = f
		e, err := jsonata.Compile(mp.Expr)
		if err != nil {
			errs = append(errs, fmt.Errorf("mapping %d (%s): %w", i, mp.Target, err))
			continue
		}
		if err := e.RegisterExts(customFuncs()); err != nil {
			return nil, err
		}
		cm.expr = e
		if mp.When != "" {
			ast, iss := env.Compile(mp.When)
			if iss.Err() != nil {
				errs = append(errs, fmt.Errorf("mapping %d (%s) when: %w", i, mp.Target, iss.Err()))
				continue
			}
			if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
				errs = append(errs, fmt.Errorf("mapping %d (%s) when: must be bool, got %s", i, mp.Target, ast.OutputType()))
				continue
			}
			prg, err := env.Program(ast)
			if err != nil {
				errs = append(errs, fmt.Errorf("mapping %d (%s) when: %w", i, mp.Target, err))
				continue
			}
			cm.when = prg
		}
		m.mappings = append(m.mappings, cm)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// Execute runs the map over an already parsed source value, returning the target value.
func (m *Map) Execute(src any) (any, error) {
	out := map[string]any{}
	for _, cm := range m.mappings {
		name := strings.Join(cm.path, ".")
		if cm.when != nil {
			v, _, err := cm.when.Eval(map[string]any{"src": src})
			if err != nil {
				return nil, fmt.Errorf("maps: %s when: %w", name, err)
			}
			if b, ok := v.Value().(bool); !ok || !b {
				continue
			}
		}
		v, err := cm.expr.Eval(src)
		if err != nil {
			if errors.Is(err, jsonata.ErrUndefined) {
				if cm.target.Required {
					return nil, fmt.Errorf("maps: %s: required value undefined", name)
				}
				continue
			}
			return nil, fmt.Errorf("maps: %s: %w", name, err)
		}
		if v, err = coerce(cm.target, v); err != nil {
			return nil, fmt.Errorf("maps: %s: %w", name, err)
		}
		cur := out
		for _, seg := range cm.path[:len(cm.path)-1] {
			next, ok := cur[seg].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[seg] = next
			}
			cur = next
		}
		cur[cm.path[len(cm.path)-1]] = v
	}
	return out, nil
}

// ExecuteBytes parses a source payload, runs the map and serializes the target.
func (m *Map) ExecuteBytes(payload []byte) ([]byte, error) {
	src, err := m.def.Source.ParseBytes(payload)
	if err != nil {
		return nil, err
	}
	tgt, err := m.Execute(src)
	if err != nil {
		return nil, err
	}
	return m.def.Target.Serialize(tgt)
}

func coerce(f *Field, v any) (any, error) {
	if f.Repeated {
		if _, ok := v.([]any); !ok {
			if v == nil {
				return []any{}, nil
			}
			return []any{v}, nil
		}
		return v, nil
	}
	switch f.Type {
	case String:
		if _, ok := v.(string); !ok {
			return nil, fmt.Errorf("expected string, got %T", v)
		}
	case Number:
		if _, ok := v.(float64); !ok {
			return nil, fmt.Errorf("expected number, got %T", v)
		}
	case Boolean:
		if _, ok := v.(bool); !ok {
			return nil, fmt.Errorf("expected boolean, got %T", v)
		}
	}
	return v, nil
}
