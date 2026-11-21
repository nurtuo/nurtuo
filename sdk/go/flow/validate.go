// SPDX-License-Identifier: Apache-2.0

package flow

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed flow.v1.schema.json
var schemaJSON []byte

// SchemaJSON returns the embedded flow.v1 JSON Schema.
func SchemaJSON() []byte { return append([]byte(nil), schemaJSON...) }

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic mirrors the TypeScript Diagnostic. Severity, Code, Path and StepID are compared by
// the conformance corpus; Message is free text.
type Diagnostic struct {
	Severity Severity
	Code     string // e.g. "edge.target_unknown", "schema.required"
	Message  string
	Path     string // JSON Pointer; "" is the root. May point at a not-yet-existing edge.
	StepID   string // empty when not tied to a step
}

// HasErrors reports whether any diagnostic is an error (warnings do not make a flow invalid).
func HasErrors(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

var stepPathRe = regexp.MustCompile(`^/steps/(\d+)(/|$)`)

// ValidateJSON validates a flow document. The error is non-nil only if raw is not valid JSON;
// everything else is reported as diagnostics, sorted by (path, code, stepId). Stage 1 checks the
// JSON Schema (codes "schema.*"); if it reports errors, the semantic stage is skipped.
func ValidateJSON(raw []byte) ([]Diagnostic, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("flow: invalid json: %w", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return nil, fmt.Errorf("flow: embedded schema: %w", err)
	}
	var out []Diagnostic
	var issues []rawIssue
	(&schemaChecker{root: schema}).check(schema, doc, "", &issues)
	if len(issues) > 0 {
		var steps []any
		if m, ok := doc.(map[string]any); ok {
			steps, _ = m["steps"].([]any)
		}
		for _, r := range issues {
			d := Diagnostic{Severity: SeverityError, Code: r.code, Message: r.msg, Path: r.path}
			if m := stepPathRe.FindStringSubmatch(r.path); m != nil {
				if i, err := strconv.Atoi(m[1]); err == nil && i < len(steps) {
					if s, ok := steps[i].(map[string]any); ok {
						if id, ok := s["id"].(string); ok {
							d.StepID = id
						}
					}
				}
			}
			out = append(out, d)
		}
	} else {
		var f Flow
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("flow: decode: %w", err)
		}
		out = semantic(&f)
	}
	return finish(out), nil
}

func finish(diags []Diagnostic) []Diagnostic {
	seen := map[[4]string]bool{}
	uniq := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		k := [4]string{string(d.Severity), d.Code, d.Path, d.StepID}
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, d)
		}
	}
	sort.SliceStable(uniq, func(i, j int) bool {
		a, b := uniq[i], uniq[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.StepID < b.StepID
	})
	return uniq
}

func semantic(f *Flow) []Diagnostic {
	var out []Diagnostic
	add := func(sev Severity, code, path, msg, stepID string) {
		out = append(out, Diagnostic{Severity: sev, Code: code, Path: path, Message: msg, StepID: stepID})
	}
	edgePath := func(key string) string { return "/edges/" + escapePointer(key) }

	first := map[string]*Step{}
	for i := range f.Steps {
		s := &f.Steps[i]
		if _, dup := first[s.ID]; dup {
			add(SeverityError, "step.duplicate_id", fmt.Sprintf("/steps/%d", i), "Duplicate step id "+s.ID, s.ID)
		} else {
			first[s.ID] = s
		}
	}

	var starts []int
	for i, s := range f.Steps {
		if s.Kind == KindStart {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		add(SeverityError, "flow.start_count", "/steps", "Flow has no start step", "")
	}
	for _, i := range starts[min(1, len(starts)):] {
		add(SeverityError, "flow.start_count", fmt.Sprintf("/steps/%d", i), "Flow has more than one start step", f.Steps[i].ID)
	}

	adj := map[string][]string{}
	for key, target := range f.Edges {
		c := strings.Index(key, ":")
		src, port := key[:c], key[c+1:]
		path := edgePath(key)
		srcStep, tgtStep := first[src], first[target]
		srcID := ""
		if srcStep == nil {
			add(SeverityError, "edge.source_unknown", path, fmt.Sprintf("Edge %s has unknown source step %s", key, src), "")
		} else {
			srcID = src
			valid := false
			for _, p := range PortsOf(*srcStep) {
				if p == port {
					valid = true
				}
			}
			if !valid {
				add(SeverityError, "edge.port_invalid", path, fmt.Sprintf("Step %s (%s) has no port %s", src, srcStep.Kind, port), src)
			}
		}
		if tgtStep == nil {
			add(SeverityError, "edge.target_unknown", path, fmt.Sprintf("Edge %s points to unknown step %s", key, target), srcID)
		} else if tgtStep.Kind == KindStart {
			add(SeverityError, "start.has_incoming", path, fmt.Sprintf("Start step %s cannot have incoming edges", target), target)
		}
		if srcStep != nil && tgtStep != nil {
			adj[src] = append(adj[src], target)
		}
	}

	for i, s := range f.Steps {
		at := fmt.Sprintf("/steps/%d", i)
		for _, p := range PortsOf(s) {
			if _, ok := f.Edges[s.ID+":"+p]; !ok {
				add(SeverityError, "step.port_unconnected", edgePath(s.ID+":"+p), fmt.Sprintf("Port %s of step %s is not connected", p, s.ID), s.ID)
			}
		}
		switch s.Kind {
		case KindRoute:
			var c RouteConfig
			_ = json.Unmarshal(s.Config, &c)
			seen := map[string]bool{}
			for j, r := range c.Routes {
				p := fmt.Sprintf("%s/config/routes/%d/id", at, j)
				if r.ID == "default" {
					add(SeverityError, "route.reserved_id", p, "Route id 'default' is reserved", s.ID)
				} else if seen[r.ID] {
					add(SeverityError, "route.duplicate_id", p, "Duplicate route id "+r.ID, s.ID)
				}
				seen[r.ID] = true
			}
		case KindSetProperties:
			var c SetPropertiesConfig
			_ = json.Unmarshal(s.Config, &c)
			seen := map[[2]string]bool{}
			for j, p := range c.Properties {
				scope := p.Scope
				if scope == "" {
					scope = "document"
				}
				k := [2]string{scope, p.Name}
				if seen[k] {
					add(SeverityError, "setProperties.duplicate_name", fmt.Sprintf("%s/config/properties/%d/name", at, j), fmt.Sprintf("Property %s is set twice in the same scope", p.Name), s.ID)
				}
				seen[k] = true
			}
		case KindProcessCall:
			var c ProcessCallConfig
			_ = json.Unmarshal(s.Config, &c)
			if c.Flow == f.ID {
				add(SeverityError, "processCall.self_reference", at+"/config/flow", "A flow cannot call itself", s.ID)
			}
		}
	}

	// Cycles: the M1 engine interprets a DAG. A step is on a cycle iff it can reach itself.
	reach := func(from string) map[string]bool {
		seen := map[string]bool{}
		stack := append([]string(nil), adj[from]...)
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, adj[n]...)
		}
		return seen
	}
	for i, s := range f.Steps {
		if reach(s.ID)[s.ID] {
			add(SeverityError, "flow.cycle", fmt.Sprintf("/steps/%d", i), fmt.Sprintf("Step %s is part of a cycle", s.ID), s.ID)
		}
	}

	if len(starts) > 0 {
		reached := map[string]bool{}
		for _, i := range starts {
			id := f.Steps[i].ID
			reached[id] = true
			for n := range reach(id) {
				reached[n] = true
			}
		}
		for i, s := range f.Steps {
			if !reached[s.ID] {
				add(SeverityWarning, "step.unreachable", fmt.Sprintf("/steps/%d", i), fmt.Sprintf("Step %s is not reachable from the start step", s.ID), s.ID)
			}
		}
	}
	return out
}
