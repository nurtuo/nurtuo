// SPDX-License-Identifier: AGPL-3.0-only

package maps

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var srcJSON = &Profile{Name: "emp", Format: JSON, Root: &Field{Type: Object, Children: []*Field{
	{Name: "first", Type: String, Required: true},
	{Name: "last", Type: String},
	{Name: "age", Type: Number},
	{Name: "tags", Type: String, Repeated: true},
	{Name: "addr", Type: Object, Children: []*Field{{Name: "city", Type: String}}},
}}}

var tgtJSON = &Profile{Name: "out", Format: JSON, Root: &Field{Type: Object, Children: []*Field{
	{Name: "name", Type: String, Required: true},
	{Name: "adult", Type: Boolean},
	{Name: "city", Type: String},
	{Name: "id", Type: String},
	{Name: "loc", Type: Object, Children: []*Field{{Name: "town", Type: String}}},
}}}

var xmlProfile = &Profile{Name: "x", Format: XML, Root: &Field{Name: "emp", Type: Object, Children: []*Field{
	{Name: "id", Type: String, Attr: true, Required: true},
	{Name: "name", Type: String, Required: true},
	{Name: "age", Type: Number},
	{Name: "phone", Type: String, Repeated: true},
}}}

func TestParseJSON(t *testing.T) {
	tests := []struct {
		name, in, wantErr string
	}{
		{"ok", `{"first":"A","age":3,"tags":["x"]}`, ""},
		{"missing required", `{"last":"B"}`, "required field missing"},
		{"wrong type", `{"first":"A","age":"x"}`, "expected number"},
		{"not array", `{"first":"A","tags":"x"}`, "expected array"},
		{"bad json", `{`, "invalid json"},
		{"nested wrong", `{"first":"A","addr":{"city":1}}`, "expected string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srcJSON.ParseBytes([]byte(tc.in))
			check(t, err, tc.wantErr)
		})
	}
}

func TestParseXML(t *testing.T) {
	v, err := xmlProfile.ParseBytes([]byte(`<emp id="7"><name>Ann</name><age> 41 </age><phone>1</phone><phone>2</phone></emp>`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["id"] != "7" || m["age"] != 41.0 || len(m["phone"].([]any)) != 2 {
		t.Fatalf("got %v", m)
	}
	tests := []struct{ name, in, wantErr string }{
		{"wrong root", `<x/>`, "profile expects"},
		{"missing attr", `<emp><name>a</name></emp>`, "required attribute"},
		{"missing elem", `<emp id="1"/>`, "required element"},
		{"bad number", `<emp id="1"><name>a</name><age>zz</age></emp>`, "not a number"},
		{"undeclared repeat", `<emp id="1"><name>a</name><name>b</name></emp>`, "not declared repeated"},
		{"bad xml", `<emp`, "invalid xml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := xmlProfile.ParseBytes([]byte(tc.in))
			check(t, err, tc.wantErr)
		})
	}
}

func TestProfileValidate(t *testing.T) {
	bad := []*Profile{
		nil,
		{Format: JSON},
		{Format: "csv", Root: &Field{Type: Object}},
		{Format: XML, Root: &Field{Type: Object}},
		{Format: JSON, Root: &Field{Type: "blob"}},
		{Format: JSON, Root: &Field{Type: Object, Children: []*Field{{Name: "a", Type: String}, {Name: "a", Type: String}}}},
		{Format: JSON, Root: &Field{Type: String, Children: []*Field{{Name: "a", Type: String}}}},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestMapExecute(t *testing.T) {
	def := Definition{Source: srcJSON, Target: tgtJSON, Mappings: []Mapping{
		{Target: "name", Expr: `$uppercase(first) & " " & $default(last, "-")`},
		{Target: "adult", Expr: `age >= 18`, When: `has(src.age)`},
		{Target: "city", Expr: `addr.city`},
		{Target: "id", Expr: `$padLeft($string($default(age, 0)), 5, "0")`},
		{Target: "loc.town", Expr: `addr.city`, When: `src.first == "Ann"`},
	}}
	m, err := Compile(def)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, in, want string }{
		{"full", `{"first":"Ann","last":"Lee","age":20,"addr":{"city":"Oslo"}}`,
			`{"adult":true,"city":"Oslo","id":"00020","loc":{"town":"Oslo"},"name":"ANN Lee"}`},
		{"sparse", `{"first":"Bob"}`, `{"id":"00000","name":"BOB -"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.ExecuteBytes([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
	if _, err := m.ExecuteBytes([]byte(`{"last":"x"}`)); err == nil {
		t.Fatal("expected required error")
	}
}

func TestMapToXML(t *testing.T) {
	tgt := &Profile{Format: XML, Root: &Field{Name: "out", Type: Object, Children: []*Field{
		{Name: "ref", Type: String, Attr: true}, {Name: "n", Type: String},
	}}}
	m, err := Compile(Definition{Source: srcJSON, Target: tgt, Mappings: []Mapping{
		{Target: "ref", Expr: `"r1"`}, {Target: "n", Expr: `first & "<"`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.ExecuteBytes([]byte(`{"first":"A"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `<out ref="r1"><n>A&lt;</n></out>` {
		t.Fatalf("got %s", got)
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name    string
		mp      Mapping
		wantErr string
	}{
		{"unknown target", Mapping{Target: "nope", Expr: `first`}, "not in target profile"},
		{"bad jsonata", Mapping{Target: "name", Expr: `first &&& (`}, "mapping 0"},
		{"bad cel", Mapping{Target: "name", Expr: `first`, When: `src.x ==`}, "when"},
		{"non bool cel", Mapping{Target: "name", Expr: `first`, When: `1 + 1`}, "must be bool"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(Definition{Source: srcJSON, Target: tgtJSON, Mappings: []Mapping{tc.mp}})
			check(t, err, tc.wantErr)
		})
	}
	if _, err := Compile(Definition{Source: nil, Target: tgtJSON}); err == nil {
		t.Fatal("expected profile error")
	}
}

func TestExecuteTypeMismatch(t *testing.T) {
	m, err := Compile(Definition{Source: srcJSON, Target: tgtJSON, Mappings: []Mapping{{Target: "adult", Expr: `first`}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.ExecuteBytes([]byte(`{"first":"A"}`))
	check(t, err, "expected boolean")
}

func TestScriptStub(t *testing.T) {
	var s ScriptStep = UnavailableScript{}
	if _, err := s.Run(context.Background(), nil); !errors.Is(err, ErrNoRuntime) {
		t.Fatal(err)
	}
}

func check(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %v, want containing %q", err, want)
	}
}
