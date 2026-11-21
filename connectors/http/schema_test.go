// SPDX-License-Identifier: Apache-2.0
package http

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// The connection settings the connector reads must be declared in the published schema,
// otherwise catalog-driven UIs and the MCP server cannot offer them.
func TestConnectionSchemaDeclaresOpenAPISettings(t *testing.T) {
	b, err := os.ReadFile("schemas/connection.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"baseUrl", "openApiSpec", "openApiUrl"} {
		if _, ok := s.Properties[k]; !ok {
			t.Errorf("connection.json does not declare %q", k)
		}
	}
}

// Describe returns its own copy of the schema; it must declare the same OpenAPI settings.
func TestDescribeSchemaDeclaresOpenAPISettings(t *testing.T) {
	d, err := New().Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	props := d.GetConnectionSchema().GetFields()["properties"].GetStructValue().GetFields()
	for _, k := range []string{"baseUrl", "openApiSpec", "openApiUrl"} {
		if props[k] == nil {
			t.Errorf("Describe connection schema does not declare %q", k)
		}
	}
}
