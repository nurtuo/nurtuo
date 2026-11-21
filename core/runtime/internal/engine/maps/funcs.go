// SPDX-License-Identifier: AGPL-3.0-only

package maps

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/blues/jsonata-go"
	"github.com/blues/jsonata-go/jtypes"
)

// customFuncs are the M1 helper functions that the JSONata library lacks
// (the library already provides $string, $number, $uppercase, $substring, $join,
// $sum, $count, $now, $replace, $split, $trim, $contains and friends).
func customFuncs() map[string]jsonata.Extension {
	keepUndefined := jtypes.ArgHandler(func([]reflect.Value) bool { return false })
	return map[string]jsonata.Extension{
		// $default(value, fallback): fallback when value is undefined, null or "".
		"default": {Func: func(v, fallback any) any {
			if v == nil || v == "" {
				return fallback
			}
			return v
		}, UndefinedHandler: keepUndefined},
		// $padLeft(str, width, pad)
		"padLeft": {Func: func(s string, width int, pad string) (string, error) {
			if pad == "" {
				return "", fmt.Errorf("padLeft: empty pad")
			}
			for len([]rune(s)) < width {
				s = pad + s
			}
			return s, nil
		}},
		// $isEmpty(value)
		"isEmpty": {Func: func(v any) bool {
			switch t := v.(type) {
			case nil:
				return true
			case string:
				return strings.TrimSpace(t) == ""
			case []any:
				return len(t) == 0
			}
			return false
		}, UndefinedHandler: keepUndefined},
	}
}
