// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"fmt"
	"sync"

	"cel.dev/cel-go/cel"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
)

// CELEvaluator evaluates Decision/Route conditions with cel-go. Variables:
//
//	doc.id, doc.props (map<string,string>), process.props (map<string,string>)
//
// Referencing a property that is not set is an evaluation error (use `'k' in doc.props`).
// Templates are delegated to BasicEvaluator. Safe for concurrent use.
type CELEvaluator struct {
	once  sync.Once
	env   *cel.Env
	err   error
	cache sync.Map // expr -> cel.Program
}

func (c *CELEvaluator) init() {
	c.once.Do(func() {
		c.env, c.err = cel.NewEnv(
			cel.Variable("doc", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable("process", cel.MapType(cel.StringType, cel.DynType)))
	})
}

func (c *CELEvaluator) program(expr string) (cel.Program, error) {
	c.init()
	if c.err != nil {
		return nil, c.err
	}
	if p, ok := c.cache.Load(expr); ok {
		return p.(cel.Program), nil
	}
	ast, iss := c.env.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if t := ast.OutputType(); t != cel.BoolType && t != cel.DynType {
		return nil, fmt.Errorf("condition %q is %s, not bool", expr, t)
	}
	p, err := c.env.Program(ast)
	if err != nil {
		return nil, err
	}
	c.cache.Store(expr, p)
	return p, nil
}

// Condition implements Evaluator.
func (c *CELEvaluator) Condition(expr string, d *docs.Document, p *Process) (bool, error) {
	prg, err := c.program(expr)
	if err != nil {
		return false, err
	}
	out, _, err := prg.Eval(map[string]any{
		"doc":     map[string]any{"id": d.ID, "props": strMap(d.Props)},
		"process": map[string]any{"props": strMap(p.Props())},
	})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("condition %q is not boolean", expr)
	}
	return b, nil
}

// Template implements Evaluator.
func (*CELEvaluator) Template(t string, d *docs.Document, p *Process) (string, error) {
	return BasicEvaluator{}.Template(t, d, p)
}

func strMap(m map[string]string) map[string]any {
	o := make(map[string]any, len(m))
	for k, v := range m {
		o[k] = v
	}
	return o
}
