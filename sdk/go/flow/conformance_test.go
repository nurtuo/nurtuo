// SPDX-License-Identifier: Apache-2.0

package flow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// The corpus is shared with the TypeScript validator (sdk/ts/flow-validate). Both must produce
// identical diagnostics (severity, code, path, stepId) for every file.
const corpusDir = "../../ts/flow-schema/testdata/conformance"
const examplesDir = "../../../examples/flows"

type expected struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Path     string `json:"path"`
	StepID   string `json:"stepId"`
}

type corpusCase struct {
	Description string          `json:"description"`
	Flow        json.RawMessage `json:"flow"`
	Expect      []expected      `json:"expect"`
}

func TestConformanceCorpus(t *testing.T) {
	for _, dir := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(corpusDir, dir, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no corpus files in %s: %v", dir, err)
		}
		for _, f := range files {
			f, dir := f, dir
			t.Run(dir+"/"+filepath.Base(f), func(t *testing.T) {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				var c corpusCase
				if err := json.Unmarshal(raw, &c); err != nil {
					t.Fatal(err)
				}
				got, err := flow.ValidateJSON(c.Flow)
				if err != nil {
					t.Fatalf("ValidateJSON: %v", err)
				}
				var gotP, wantP []expected
				for _, d := range got {
					gotP = append(gotP, expected{string(d.Severity), d.Code, d.Path, d.StepID})
				}
				wantP = append(wantP, c.Expect...)
				if len(gotP) != len(wantP) {
					t.Fatalf("diagnostics mismatch\n got: %+v\nwant: %+v", gotP, wantP)
				}
				for i := range gotP {
					if gotP[i] != wantP[i] {
						t.Fatalf("diagnostic %d mismatch\n got: %+v\nwant: %+v", i, gotP, wantP)
					}
				}
				if flow.HasErrors(got) != (dir == "invalid") {
					t.Fatalf("HasErrors=%v in %s", flow.HasErrors(got), dir)
				}
			})
		}
	}
}

func TestExamplesHaveNoErrors(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(examplesDir, "*.flow.json"))
	if len(files) == 0 {
		t.Fatal("no example flows found")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		d, err := flow.ValidateJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if flow.HasErrors(d) {
			t.Errorf("%s: %+v", f, d)
		}
	}
}

func TestSchemaCopyInSync(t *testing.T) {
	want, err := os.ReadFile("../../ts/flow-schema/schema/flow.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("flow.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Fatal("sdk/go/flow/flow.v1.schema.json differs from the TS schema; run `pnpm --filter @nurtuo/flow-schema run sync`")
	}
}

func TestParseDecodesTypedFlow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(examplesDir, "get-employee.flow.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, diags, err := flow.Parse(raw)
	if err != nil || flow.HasErrors(diags) {
		t.Fatalf("Parse: %v %+v", err, diags)
	}
	if f.ID != "get-employee" || f.Revision != 1 || len(f.Steps) != 6 || f.Edges["found:true"] != "to_response" {
		t.Fatalf("unexpected decode: %+v", f)
	}
	cfg, err := f.Steps[0].TypedConfig()
	if err != nil {
		t.Fatal(err)
	}
	sc, ok := cfg.(flow.StartConfig)
	if !ok || sc.Type != "listener" || sc.Connector != "http" || sc.Path != "/employees/{id}" {
		t.Fatalf("start config: %#v", cfg)
	}
	cfg, _ = f.Steps[5].TypedConfig()
	if st, ok := cfg.(flow.StopConfig); !ok || st.Status == nil || *st.Status != 404 {
		t.Fatalf("stop config: %#v", cfg)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	_, diags, err := flow.Parse([]byte(`{"schema":"x"}`))
	if err == nil || !flow.HasErrors(diags) {
		t.Fatalf("expected error, got %v %+v", err, diags)
	}
	if _, _, err := flow.Parse([]byte(`not json`)); err == nil || !strings.Contains(err.Error(), "json") {
		t.Fatalf("expected json error, got %v", err)
	}
}

func TestPortsOf(t *testing.T) {
	mk := func(kind, cfg string) flow.Step {
		return flow.Step{ID: "a", Kind: kind, Config: json.RawMessage(cfg)}
	}
	cases := []struct {
		s    flow.Step
		want string
	}{
		{mk("decision", `{}`), "true,false"},
		{mk("branch", `{"paths":3}`), "1,2,3"},
		{mk("route", `{"routes":[{"id":"x","cel":"1"}]}`), "x,default"},
		{mk("tryCatch", `{}`), "try,catch"},
		{mk("stop", `{}`), ""},
		{mk("message", `{}`), "default"},
	}
	for _, c := range cases {
		if got := strings.Join(flow.PortsOf(c.s), ","); got != c.want {
			t.Errorf("%s: got %q want %q", c.s.Kind, got, c.want)
		}
	}
}
