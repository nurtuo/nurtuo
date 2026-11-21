// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// A deliberately small interpreter for the subset of JSON Schema 2020-12 that flow.v1 uses; the
// twin of sdk/ts/flow-validate/src/jsonschema.ts (keep them in lockstep). Supported keywords:
// $ref (#/$defs/..), type, const, enum, minLength, pattern, minimum, maximum, minItems, items,
// required, properties, additionalProperties, propertyNames, allOf, if/then. A failing `type`
// stops evaluation of the remaining keywords of that schema node.

type rawIssue struct {
	code string
	path string
	msg  string
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}

var (
	regexMu    sync.Mutex
	regexCache = map[string]*regexp.Regexp{}
)

func compile(p string) *regexp.Regexp {
	regexMu.Lock()
	defer regexMu.Unlock()
	if re, ok := regexCache[p]; ok {
		return re
	}
	re := regexp.MustCompile(p)
	regexCache[p] = re
	return re
}

type schemaChecker struct {
	root map[string]any
}

func (c *schemaChecker) resolve(ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		panic(fmt.Sprintf("flow: unsupported $ref %s", ref))
	}
	var cur any = c.root
	for _, part := range strings.Split(ref[2:], "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			panic("flow: unresolved $ref " + ref)
		}
		cur = m[part]
	}
	m, ok := cur.(map[string]any)
	if !ok {
		panic("flow: unresolved $ref " + ref)
	}
	return m
}

func (c *schemaChecker) check(schema map[string]any, v any, path string, sink *[]rawIssue) {
	add := func(code, msg, at string) {
		*sink = append(*sink, rawIssue{"schema." + code, at, msg})
	}
	if ref, ok := schema["$ref"].(string); ok {
		c.check(c.resolve(ref), v, path, sink)
	}
	if want, ok := schema["type"].(string); ok {
		got := jsonType(v)
		if want != got && (want != "number" || got != "integer") {
			add("type", fmt.Sprintf("expected %s, got %s", want, got), path)
			return
		}
	}
	if cv, ok := schema["const"]; ok && v != cv {
		add("const", fmt.Sprintf("must equal %v", cv), path)
	}
	if en, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range en {
			if e == v {
				found = true
			}
		}
		if !found {
			add("enum", "not an allowed value", path)
		}
	}
	switch x := v.(type) {
	case string:
		if n, ok := schema["minLength"].(float64); ok && float64(utf8.RuneCountInString(x)) < n {
			add("minLength", fmt.Sprintf("shorter than %v", n), path)
		}
		if p, ok := schema["pattern"].(string); ok && !compile(p).MatchString(x) {
			add("pattern", "does not match "+p, path)
		}
	case float64:
		if n, ok := schema["minimum"].(float64); ok && x < n {
			add("minimum", fmt.Sprintf("below %v", n), path)
		}
		if n, ok := schema["maximum"].(float64); ok && x > n {
			add("maximum", fmt.Sprintf("above %v", n), path)
		}
	case []any:
		if n, ok := schema["minItems"].(float64); ok && float64(len(x)) < n {
			add("minItems", fmt.Sprintf("fewer than %v items", n), path)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range x {
				c.check(items, item, fmt.Sprintf("%s/%d", path, i), sink)
			}
		}
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				name := r.(string)
				if _, has := x[name]; !has {
					add("required", "missing required property "+name, path+"/"+escapePointer(name))
				}
			}
		}
		for k, sub := range props {
			if val, has := x[k]; has {
				c.check(sub.(map[string]any), val, path+"/"+escapePointer(k), sink)
			}
		}
		for k, val := range x {
			at := path + "/" + escapePointer(k)
			if pn, ok := schema["propertyNames"].(map[string]any); ok {
				c.check(pn, k, at, sink)
			}
			if _, declared := props[k]; declared {
				continue
			}
			switch ap := schema["additionalProperties"].(type) {
			case bool:
				if !ap {
					add("additionalProperties", "unknown property "+k, at)
				}
			case map[string]any:
				c.check(ap, val, at, sink)
			}
		}
	}
	if all, ok := schema["allOf"].([]any); ok {
		for _, sub := range all {
			c.check(sub.(map[string]any), v, path, sink)
		}
	}
	if cond, ok := schema["if"].(map[string]any); ok {
		var probe []rawIssue
		c.check(cond, v, path, &probe)
		if then, ok := schema["then"].(map[string]any); ok && len(probe) == 0 {
			c.check(then, v, path, sink)
		}
	}
}
