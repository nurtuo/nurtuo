// SPDX-License-Identifier: Apache-2.0

// Package flow is the Go side of the Nurtuo flow format (nurtuo.flow/v1): typed structs for the
// engine, and a validator that produces exactly the same diagnostics as the TypeScript validator
// (@nurtuo/flow-validate). The JSON Schema (flow.v1.schema.json, copied from
// sdk/ts/flow-schema/schema by `pnpm --filter @nurtuo/flow-schema run sync`) is the source of
// truth; the structs here are kept consistent with it by the shared conformance corpus.
package flow

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// SchemaVersion is the value of Flow.Schema.
const SchemaVersion = "nurtuo.flow/v1"

// Step kinds.
const (
	KindStart           = "start"
	KindConnector       = "connector"
	KindQueue           = "queue"
	KindMap             = "map"
	KindScript          = "script"
	KindMessage         = "message"
	KindSetProperties   = "setProperties"
	KindDecision        = "decision"
	KindBranch          = "branch"
	KindRoute           = "route"
	KindTryCatch        = "tryCatch"
	KindProcessCall     = "processCall"
	KindNotify          = "notify"
	KindStop            = "stop"
	KindReturnDocuments = "returnDocuments"
)

// Flow is one flow document. Edges are keyed by "stepId:port"; one outgoing edge per port.
type Flow struct {
	Schema      string            `json:"schema"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Revision    int               `json:"revision"`
	Steps       []Step            `json:"steps"`
	Edges       map[string]string `json:"edges"`
}

// Step is one node. Config is kind-specific; use TypedConfig for a decoded value.
type Step struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Label  string          `json:"label,omitempty"`
	Config json.RawMessage `json:"config"`
}

type StartConfig struct {
	Type      string `json:"type"`
	Connector string `json:"connector,omitempty"`
	Path      string `json:"path,omitempty"`
	Method    string `json:"method,omitempty"`
	Queue     string `json:"queue,omitempty"`
	Cron      string `json:"cron,omitempty"`
}

type ConnectorConfig struct {
	Connector  string `json:"connector"`
	Operation  string `json:"operation"`
	ObjectType string `json:"objectType,omitempty"`
	Connection string `json:"connection,omitempty"`
}

type QueueConfig struct {
	Queue     string `json:"queue"`
	Operation string `json:"operation"`
}

type MapConfig struct {
	JSONata string `json:"jsonata"`
}

// ScriptConfig runs a sandboxed WASM script; Script names a registered script module.
type ScriptConfig struct {
	Script       string   `json:"script"`
	TimeoutMs    int      `json:"timeoutMs,omitempty"`
	MemoryMiB    int      `json:"memoryMiB,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type MessageConfig struct {
	Template string `json:"template"`
}

type PropertyAssignment struct {
	Name  string `json:"name"`
	Scope string `json:"scope,omitempty"` // "document" (default) or "process"
	Value string `json:"value"`
}

type SetPropertiesConfig struct {
	Properties []PropertyAssignment `json:"properties"`
}

type DecisionConfig struct {
	CEL string `json:"cel"`
}

type BranchConfig struct {
	Paths int `json:"paths"`
}

type Route struct {
	ID  string `json:"id"`
	CEL string `json:"cel"`
}

type RouteConfig struct {
	Routes []Route `json:"routes"`
}

type TryCatchConfig struct {
	Retries int `json:"retries,omitempty"`
}

type ProcessCallConfig struct {
	Flow     string `json:"flow"`
	Revision *int   `json:"revision,omitempty"`
}

type NotifyConfig struct {
	Channel  string `json:"channel"`
	Template string `json:"template,omitempty"`
}

type StopConfig struct {
	Status  *int   `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

type ReturnDocumentsConfig struct{}

// TypedConfig decodes Config into the struct for the step's kind (StartConfig, StopConfig, ...).
func (s Step) TypedConfig() (any, error) {
	var dst any
	switch s.Kind {
	case KindStart:
		dst = &StartConfig{}
	case KindConnector:
		dst = &ConnectorConfig{}
	case KindQueue:
		dst = &QueueConfig{}
	case KindMap:
		dst = &MapConfig{}
	case KindScript:
		dst = &ScriptConfig{}
	case KindMessage:
		dst = &MessageConfig{}
	case KindSetProperties:
		dst = &SetPropertiesConfig{}
	case KindDecision:
		dst = &DecisionConfig{}
	case KindBranch:
		dst = &BranchConfig{}
	case KindRoute:
		dst = &RouteConfig{}
	case KindTryCatch:
		dst = &TryCatchConfig{}
	case KindProcessCall:
		dst = &ProcessCallConfig{}
	case KindNotify:
		dst = &NotifyConfig{}
	case KindStop:
		dst = &StopConfig{}
	case KindReturnDocuments:
		dst = &ReturnDocumentsConfig{}
	default:
		return nil, fmt.Errorf("flow: unknown step kind %q", s.Kind)
	}
	if len(s.Config) > 0 {
		if err := json.Unmarshal(s.Config, dst); err != nil {
			return nil, fmt.Errorf("flow: step %s config: %w", s.ID, err)
		}
	}
	switch v := dst.(type) {
	case *StartConfig:
		return *v, nil
	case *ConnectorConfig:
		return *v, nil
	case *QueueConfig:
		return *v, nil
	case *MapConfig:
		return *v, nil
	case *ScriptConfig:
		return *v, nil
	case *MessageConfig:
		return *v, nil
	case *SetPropertiesConfig:
		return *v, nil
	case *DecisionConfig:
		return *v, nil
	case *BranchConfig:
		return *v, nil
	case *RouteConfig:
		return *v, nil
	case *TryCatchConfig:
		return *v, nil
	case *ProcessCallConfig:
		return *v, nil
	case *NotifyConfig:
		return *v, nil
	case *StopConfig:
		return *v, nil
	default:
		return *(v.(*ReturnDocumentsConfig)), nil
	}
}

// PortsOf returns the output ports of a step in canonical order. Terminal kinds have none.
func PortsOf(s Step) []string {
	switch s.Kind {
	case KindDecision:
		return []string{"true", "false"}
	case KindTryCatch:
		return []string{"try", "catch"}
	case KindStop, KindReturnDocuments:
		return nil
	case KindBranch:
		var c BranchConfig
		_ = json.Unmarshal(s.Config, &c)
		ports := make([]string, 0, c.Paths)
		for i := 1; i <= c.Paths; i++ {
			ports = append(ports, strconv.Itoa(i))
		}
		return ports
	case KindRoute:
		var c RouteConfig
		_ = json.Unmarshal(s.Config, &c)
		var ports []string
		seen := map[string]bool{"default": true}
		for _, r := range c.Routes {
			if !seen[r.ID] {
				seen[r.ID] = true
				ports = append(ports, r.ID)
			}
		}
		return append(ports, "default")
	default:
		return []string{"default"}
	}
}

// Parse validates raw JSON and, if it has no errors, decodes it. Warnings are returned with a
// nil error. If validation finds errors the returned Flow is nil and err is non-nil; the
// diagnostics are still returned.
func Parse(raw []byte) (*Flow, []Diagnostic, error) {
	diags, err := ValidateJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	if HasErrors(diags) {
		return nil, diags, fmt.Errorf("flow: invalid flow (%d diagnostics)", len(diags))
	}
	var f Flow
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, diags, fmt.Errorf("flow: decode: %w", err)
	}
	return &f, diags, nil
}
