// SPDX-License-Identifier: AGPL-3.0-only

package maps

import (
	"errors"

	"github.com/blues/jsonata-go"
)

// Transform is a compiled whole-document JSONata expression with the maps helper functions
// ($default, $padLeft, ...) registered. It backs the flow Map step. Safe for concurrent use.
type Transform struct{ e *jsonata.Expr }

// CompileTransform compiles expr.
func CompileTransform(expr string) (*Transform, error) {
	e, err := jsonata.Compile(expr)
	if err != nil {
		return nil, err
	}
	if err := e.RegisterExts(customFuncs()); err != nil {
		return nil, err
	}
	return &Transform{e: e}, nil
}

// Eval evaluates the expression against an already parsed source value. ok is false when the
// expression result is undefined.
func (t *Transform) Eval(src any) (out any, ok bool, err error) {
	v, err := t.e.Eval(src)
	if err != nil {
		if errors.Is(err, jsonata.ErrUndefined) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return v, true, nil
}
