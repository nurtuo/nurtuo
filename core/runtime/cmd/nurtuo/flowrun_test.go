// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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
		"no args":          {},
		"unknown flag":     {flowFile, "--nope"},
		"set without =":    {flowFile, "--set", "path"},
		"set empty key":    {flowFile, "--set", "=x"},
		"two flows":        {flowFile, flowFile},
		"negative timeout": {flowFile, "--timeout", "-1s"},
		"bad duration":     {flowFile, "--timeout", "soon"},
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
		if code != 2 || out != "" || !strings.Contains(errs, `{"error":{"code":"usage"`) || !json.Valid([]byte(errs)) {
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

// stubRun runs a flow with a manual start and one stub connection built from settings.
func stubRun(t *testing.T, settings string, extra ...string) (code int, stdout, stderr string, st *stubConnector) {
	t.Helper()
	dir := t.TempDir()
	reg, st := stubRegistry()
	flowFile := writeTemp(t, dir, "f.json", flowJSON(manualStart, "stub", "s"))
	conn := writeTemp(t, dir, "c.json", connJSON("s", "stub", settings))
	args := append([]string{flowFile, "--connection", conn}, extra...)
	code, stdout, stderr = runFlow(t, reg, args...)
	return code, stdout, stderr, st
}

func TestFlowRunPrintsReturnedPayload(t *testing.T) {
	t.Run("exact bytes, no newline", func(t *testing.T) {
		code, out, errs, _ := stubRun(t, stubSettings("payload", `"v1.2.3"`))
		if code != 0 || out != "v1.2.3" || errs != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("1 MiB", func(t *testing.T) {
		big := strings.Repeat("0123456789abcdef", 64*1024)
		code, out, errs, _ := stubRun(t, stubSettings("payload", `"`+big+`"`))
		if code != 0 || out != big || errs != "" {
			t.Fatalf("code=%d len(stdout)=%d stderr=%q", code, len(out), errs)
		}
	})
}

func TestFlowRunSetProperties(t *testing.T) {
	code, out, errs, st := stubRun(t, stubSettings(), "--set", "path=/downloads/version.txt", "--set", "a=1=2", "--set", "a=last", "--set", "empty=")
	if code != 0 || out != "/downloads/version.txt" || errs != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	if len(st.seen) != 1 {
		t.Fatalf("connector saw %d documents, want 1", len(st.seen))
	}
	p := st.seen[0]
	if p["path"] != "/downloads/version.txt" || p["a"] != "last" || p["empty"] != "" {
		t.Fatalf("properties = %v", p)
	}
	if _, ok := p["empty"]; !ok {
		t.Fatalf("empty value dropped: %v", p)
	}
}

func TestFlowRunListenerStartIgnored(t *testing.T) {
	for name, start := range map[string]string{
		"http listener":    `{"type":"listener","connector":"http","path":"/employees/{id}"}`,
		"listener no path": `{"type":"listener","connector":"http"}`,
		"queue listener":   `{"type":"queue","queue":"orders"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			reg, st := stubRegistry()
			flowFile := writeTemp(t, dir, "f.json", flowJSON(start, "stub", "s"))
			conn := writeTemp(t, dir, "c.json", connJSON("s", "stub", stubSettings("payload", `"ok"`)))
			code, out, errs := runFlow(t, reg, flowFile, "--connection", conn)
			if code != 0 || out != "ok" || errs != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
			}
			if len(st.seen) != 1 || len(st.seen[0]) != 0 {
				t.Fatalf("want one empty document, connector saw %v", st.seen)
			}
		})
	}
}

func TestFlowRunMissingSecret(t *testing.T) {
	settings := stubSettings("password", `"secret://stub_password"`, "payload", `"ok"`)
	t.Run("unset names the secret", func(t *testing.T) {
		t.Setenv("NURTUO_SECRET_STUB_PASSWORD", "")
		code, out, errs, _ := stubRun(t, settings)
		if code != 1 || out != "" || !strings.Contains(errs, `"stub_password"`) || !strings.Contains(errs, "not found") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("env resolves", func(t *testing.T) {
		t.Setenv("NURTUO_SECRET_STUB_PASSWORD", "from-env-value")
		code, out, errs, _ := stubRun(t, settings)
		if code != 0 || out != "ok" || errs != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("secrets dir resolves", func(t *testing.T) {
		t.Setenv("NURTUO_SECRET_STUB_PASSWORD", "")
		sd := filepath.Join(t.TempDir(), "secrets")
		if err := os.Mkdir(sd, 0o700); err != nil {
			t.Fatal(err)
		}
		writeTemp(t, sd, "stub_password", "from-dir-value\n")
		code, out, errs, _ := stubRun(t, settings, "--secrets-dir", sd)
		if code != 0 || out != "ok" || errs != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("literal password is a validation error", func(t *testing.T) {
		code, out, errs, _ := stubRun(t, stubSettings("password", `"hunter2-literal"`))
		if code != 2 || out != "" || !strings.Contains(errs, "secret://") || !strings.Contains(errs, "NURTUO_SECRET_") || strings.Contains(errs, "hunter2-literal") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
}

func TestFlowRunExitCodes(t *testing.T) {
	t.Run("connector error", func(t *testing.T) {
		code, out, errs, _ := stubRun(t, stubSettings("mode", `"fail"`))
		if code != 1 || out != "" || !strings.Contains(errs, "cannot connect") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("document failed", func(t *testing.T) {
		code, out, errs, _ := stubRun(t, stubSettings("mode", `"failure"`), "--set", "path=/nope.txt")
		if code != 1 || out != "" || !strings.Contains(errs, "/nope.txt") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("no_runner", func(t *testing.T) {
		script := filepath.Join("..", "..", "..", "..", "examples", "flows", "reference", "script.flow.json")
		code, out, errs := runFlow(t, nil, script)
		if code != 1 || out != "" || !strings.Contains(errs, "no runner registered") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("timeout cancels", func(t *testing.T) {
		code, out, errs, _ := stubRun(t, stubSettings("mode", `"block"`), "--timeout", "100ms")
		if code != 1 || out != "" || !strings.Contains(errs, "timed out") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("stop with status", func(t *testing.T) {
		dir := t.TempDir()
		f := writeTemp(t, dir, "f.json", `{"schema":"nurtuo.flow/v1","id":"s","name":"s","revision":1,"steps":[
 {"id":"start","kind":"start","config":{"type":"manual"}},
 {"id":"halt","kind":"stop","config":{"status":404}}],"edges":{"start:default":"halt"}}`)
		code, out, errs := runFlow(t, nil, f)
		if code != 3 || out != "" || !strings.Contains(errs, "flow stopped with status 404") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
		code, out, errs = runFlow(t, nil, f, "--json")
		if code != 3 || out != "" || !strings.Contains(errs, `"code":"stopped"`) {
			t.Fatalf("json: code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("success", func(t *testing.T) {
		if code, _, _, _ := stubRun(t, stubSettings("payload", `"x"`)); code != 0 {
			t.Fatalf("code=%d", code)
		}
	})
}

func TestFlowRunJSONEnvelope(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		code, out, errs, _ := stubRun(t, stubSettings("payload", `"hello"`), "--json", "--set", "path=/p")
		if code != 0 || errs != "" {
			t.Fatalf("code=%d stderr=%q", code, errs)
		}
		var env struct {
			Status    int `json:"status"`
			Documents []struct {
				ID         string            `json:"id"`
				Properties map[string]string `json:"properties"`
				Payload    string            `json:"payload"`
			} `json:"documents"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("stdout is not JSON: %v: %q", err, out)
		}
		if env.Status != 200 || len(env.Documents) != 1 || env.Documents[0].Payload != base64.StdEncoding.EncodeToString([]byte("hello")) ||
			env.Documents[0].Properties["path"] != "/p" {
			t.Fatalf("envelope = %+v", env)
		}
	})
	t.Run("multiple documents", func(t *testing.T) {
		code, out, _, _ := stubRun(t, stubSettings("mode", `"two"`, "payload", `"x"`), "--json")
		var env struct{ Documents []json.RawMessage }
		if err := json.Unmarshal([]byte(out), &env); code != 0 || err != nil || len(env.Documents) != 2 {
			t.Fatalf("code=%d err=%v stdout=%q", code, err, out)
		}
	})
	t.Run("error is JSON on stderr, stdout empty", func(t *testing.T) {
		code, out, errs, _ := stubRun(t, stubSettings("mode", `"fail"`), "--json")
		var e struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal([]byte(errs), &e); code != 1 || out != "" || err != nil || e.Error.Code != "execution" || !strings.Contains(e.Error.Message, "cannot connect") {
			t.Fatalf("code=%d stdout=%q stderr=%q err=%v", code, out, errs, err)
		}
	})
	t.Run("validation error is JSON", func(t *testing.T) {
		code, out, errs := runFlow(t, nil, filepath.Join(t.TempDir(), "absent.json"), "--json")
		if code != 2 || out != "" || !strings.HasPrefix(errs, `{"error":{"code":"validation"`) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
}

func TestFlowRunMultipleDocumentsRaw(t *testing.T) {
	code, out, errs, _ := stubRun(t, stubSettings("mode", `"two"`, "payload", `"x"`))
	if code != 1 || out != "" || !strings.Contains(errs, "--json") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
}

func TestFlowRunRedaction(t *testing.T) {
	t.Setenv("NURTUO_SECRET_STUB_PASSWORD", "very-secret-pw-123")
	code, out, errs, _ := stubRun(t, stubSettings("mode", `"leak"`, "password", `"secret://stub_password"`))
	if code != 1 || out != "" || strings.Contains(errs, "very-secret-pw-123") || !strings.Contains(errs, "***") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	code, out, errs, _ = stubRun(t, stubSettings("mode", `"leak"`, "password", `"secret://stub_password"`), "--json")
	if code != 1 || out != "" || strings.Contains(errs, "very-secret-pw-123") {
		t.Fatalf("json: code=%d stdout=%q stderr=%q", code, out, errs)
	}
}

func TestFlowRunStdoutStderrSeparation(t *testing.T) {
	t.Setenv("NURTUO_SECRET_STUB_PASSWORD", "another-secret-456")
	code, out, errs, _ := stubRun(t, stubSettings("payload", `"data"`, "password", `"secret://stub_password"`))
	if code != 0 || out != "data" || errs != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	code, out, errs, _ = stubRun(t, stubSettings("mode", `"fail"`))
	if code != 1 || out != "" || errs == "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
}
