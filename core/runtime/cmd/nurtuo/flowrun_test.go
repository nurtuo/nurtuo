// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
)

// writeTemp writes content to a file under dir and returns its path.
func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runFlow runs "nurtuo flow run args..." against reg (builtins when nil).
func runFlow(t *testing.T, reg *connhost.Registry, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	if reg == nil {
		reg = connhost.Builtins()
	}
	var out, errb bytes.Buffer
	code = flowRun(args, &out, &errb, reg)
	return code, out.String(), errb.String()
}

// flowJSON builds a flow with a start step, one connector step (connector/connection) and a return.
func flowJSON(startCfg, connector, connection string) string {
	conn := ""
	if connection != "" {
		conn = `,"connection":"` + connection + `"`
	}
	return `{"schema":"nurtuo.flow/v1","id":"t","name":"t","revision":1,"steps":[
 {"id":"start","kind":"start","config":` + startCfg + `},
 {"id":"call","kind":"connector","config":{"connector":"` + connector + `","operation":"GET"` + conn + `}},
 {"id":"done","kind":"returnDocuments","config":{}}],
 "edges":{"start:default":"call","call:default":"done"}}`
}

const manualStart = `{"type":"manual"}`

func connJSON(id, connector, settings string) string {
	return `{"schema":"nurtuo.connection/v1","id":"` + id + `","connector":"` + connector + `","settings":` + settings + `}`
}

func TestFlowRunUsage(t *testing.T) {
	dir := t.TempDir()
	flowFile := writeTemp(t, dir, "f.json", flowJSON(manualStart, "http", "a"))
	cases := map[string][]string{
		"no args":         {},
		"unknown flag":    {flowFile, "--nope"},
		"set without =":   {flowFile, "--set", "path"},
		"set empty key":   {flowFile, "--set", "=x"},
		"two flows":       {flowFile, flowFile},
		"negative timeout": {flowFile, "--timeout", "-1s"},
		"bad duration":    {flowFile, "--timeout", "soon"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errs := runFlow(t, nil, args...)
			if code != 2 || out != "" || !strings.Contains(errs, "Usage: nurtuo flow run") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
			}
		})
	}
	t.Run("unknown flow subcommand", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := dispatch([]string{"flow", "walk"}, &out, &errb); code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), "Usage: nurtuo flow") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errb.String())
		}
		if code := dispatch([]string{"flow"}, &out, &errb); code != 2 {
			t.Fatalf("bare flow: code=%d", code)
		}
	})
	t.Run("json usage error is json", func(t *testing.T) {
		code, out, errs := runFlow(t, nil, "--json")
		if code != 2 || out != "" || !strings.Contains(errs, `{"error":{"code":"usage"`) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
}

func TestFlowRunInvalidFlow(t *testing.T) {
	dir := t.TempDir()
	good := flowJSON(manualStart, "http", "a")
	cases := map[string]string{
		"not json":       `{`,
		"bad schema tag": strings.Replace(good, "nurtuo.flow/v1", "nurtuo.flow/v9", 1),
		"missing start":  strings.Replace(good, `"kind":"start"`, `"kind":"message"`, 1),
		"unknown edge":   strings.Replace(good, `"call:default":"done"`, `"call:default":"nowhere"`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeTemp(t, dir, "f.json", body)
			// A registry that fails the test if any connector is built.
			reg := connhost.NewRegistry()
			reg.Register("http", func() connector.Connector { t.Fatal("connector built for an invalid flow"); return nil })
			code, out, errs := runFlow(t, reg, p)
			if code != 2 || out != "" || !strings.Contains(errs, "nurtuo: ") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		code, out, _ := runFlow(t, nil, filepath.Join(dir, "absent.json"))
		if code != 2 || out != "" {
			t.Fatalf("code=%d stdout=%q", code, out)
		}
	})
}

func TestFlowRunConnectionParse(t *testing.T) {
	dir := t.TempDir()
	flowFile := writeTemp(t, dir, "f.json", flowJSON(manualStart, "http", "a"))
	cases := map[string]string{
		"wrong schema":   `{"schema":"nurtuo.connection/v9","id":"a","connector":"http"}`,
		"bad id":         connJSON("Bad Id", "http", `{}`),
		"unknown field":  `{"schema":"nurtuo.connection/v1","id":"a","connector":"http","extra":1}`,
		"bad secret ref": connJSON("a", "http", `{"baseUrl":"https://x","token":"secret://bad/name"}`),
		"not json":       `nope`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := writeTemp(t, dir, "c.json", body)
			code, out, errs := runFlow(t, nil, flowFile, "--connection", c)
			if code != 2 || out != "" || !strings.Contains(errs, "nurtuo: ") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
			}
		})
	}
	t.Run("missing connection file", func(t *testing.T) {
		code, _, _ := runFlow(t, nil, flowFile, "--connection", filepath.Join(dir, "absent.json"))
		if code != 2 {
			t.Fatalf("code=%d", code)
		}
	})
}

func TestFlowRunConnectionMismatch(t *testing.T) {
	dir := t.TempDir()
	flowFile := writeTemp(t, dir, "f.json", flowJSON(manualStart, "sftp", "a"))
	a := writeTemp(t, dir, "a.json", connJSON("a", "sftp", `{"host":"h","user":"u","password":"secret://pw"}`))
	httpConn := writeTemp(t, dir, "h.json", connJSON("a", "http", `{}`))
	other := writeTemp(t, dir, "o.json", connJSON("other", "sftp", `{}`))
	cases := map[string]struct {
		args []string
		want string
	}{
		"missing id":     {[]string{flowFile, "--connection", other}, `"a"`},
		"no connections": {[]string{flowFile}, `"a"`},
		"kind mismatch":  {[]string{flowFile, "--connection", httpConn}, "is for connector"},
		"duplicate id":   {[]string{flowFile, "--connection", a, "--connection", a}, "defined twice"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errs := runFlow(t, nil, tc.args...)
			if code != 2 || out != "" || !strings.Contains(errs, tc.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
			}
		})
	}
}
