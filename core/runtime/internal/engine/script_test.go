// SPDX-License-Identifier: AGPL-3.0-only

package engine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm/wasmtest"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

func scriptEngine(t testing.TB) *engine.Engine {
	t.Helper()
	we := wasm.NewEngine(wasm.EngineOptions{})
	t.Cleanup(func() { _ = we.Close(context.Background()) })
	mods := engine.NewScriptModules()
	mods.Register("upper", wasmtest.Upper())
	mods.Register("trap", wasmtest.Trap())
	mods.Register("spin", wasmtest.Spin())
	mods.Register("dup", wasmtest.Dup())
	mods.Register("drop", wasmtest.Drop())
	mods.Register("clock", wasmtest.ClockEcho())
	mods.Register("fail", wasmtest.Fail())
	mods.Register("junk", []byte("not wasm"))
	e := engine.New()
	e.Registry.RegisterScriptRunner(engine.NewScriptRunner(we, mods))
	return e
}

func scriptFlow(t *testing.T, cfg string) *flow.Flow {
	return mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "s:script:" + cfg, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "s", "s:default": "r"}))
}

func scriptDoc(id, body string, props map[string]string) *docs.Document {
	p, _ := docs.NewSpool(docs.SpoolConfig{}).FromBytes([]byte(body))
	return docs.New(id, props, p)
}

func scriptBody(t *testing.T, d *docs.Document) string {
	t.Helper()
	b, err := d.Payload().Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScriptStepTransformsDocuments(t *testing.T) {
	e := scriptEngine(t)
	f := scriptFlow(t, `{"script":"upper"}`)
	in := docs.NewSet(scriptDoc("a", "alpha", map[string]string{"k": "1"}), scriptDoc("b", "beta", nil))
	res, err := e.Run(context.Background(), f, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Returned.Len() != 2 || res.Failed.Len() != 0 {
		t.Fatalf("returned=%d failed=%d", res.Returned.Len(), res.Failed.Len())
	}
	if got := scriptBody(t, res.Returned.At(0)); got != "ALPHA" || res.Returned.At(0).Props["k"] != "1" {
		t.Fatalf("doc a: %q %v", got, res.Returned.At(0).Props)
	}
	if got := scriptBody(t, res.Returned.At(1)); got != "BETA" {
		t.Fatalf("doc b: %q", got)
	}
}

func TestScriptStepFailuresAreIsolatedPerDocument(t *testing.T) {
	// "trap" fails every document it sees; mixing in a pre-failed document must pass it through.
	e := scriptEngine(t)
	f := scriptFlow(t, `{"script":"trap"}`)
	pre := scriptDoc("pre", "x", nil)
	pre.Fail(&docs.Error{Message: "earlier"})
	res, err := e.Run(context.Background(), f, docs.NewSet(scriptDoc("a", "x", nil), pre, scriptDoc("b", "y", nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed.Len() != 3 || res.Returned.Len() != 0 {
		t.Fatalf("failed=%d returned=%d", res.Failed.Len(), res.Returned.Len())
	}
	for _, d := range res.Failed.Docs() {
		if d.ID == "pre" {
			if d.Err().Message != "earlier" {
				t.Fatalf("pre-failed doc error rewritten: %v", d.Err())
			}
			continue
		}
		if d.Err().Code != "script.trap" || d.Err().Step != "s" {
			t.Fatalf("%s: err = %+v", d.ID, d.Err())
		}
	}
}

func TestScriptStepErrorCodes(t *testing.T) {
	e := scriptEngine(t)
	cases := []struct{ cfg, code string }{
		{`{"script":"spin","timeoutMs":50}`, "script.timeout"},
		{`{"script":"clock"}`, "script.capability_denied"},
		{`{"script":"fail"}`, "script.script_error"},
	}
	for _, c := range cases {
		res, err := e.Run(context.Background(), scriptFlow(t, c.cfg), docs.NewSet(scriptDoc("a", "x", nil)), nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Failed.Len() != 1 || res.Failed.At(0).Err().Code != c.code {
			t.Errorf("%s: failed=%d err=%v want code %s", c.cfg, res.Failed.Len(), res.Failed.At(0).Err(), c.code)
		}
	}
}

func TestScriptStepCapabilityGrant(t *testing.T) {
	e := scriptEngine(t)
	res, err := e.Run(context.Background(), scriptFlow(t, `{"script":"clock","capabilities":["clock"]}`), docs.NewSet(scriptDoc("a", "x", nil)), nil)
	if err != nil || res.Returned.Len() != 1 {
		t.Fatalf("err=%v returned=%d", err, res.Returned.Len())
	}
}

func TestScriptStepFanOutAndDrop(t *testing.T) {
	e := scriptEngine(t)
	res, _ := e.Run(context.Background(), scriptFlow(t, `{"script":"dup"}`), docs.NewSet(scriptDoc("a", "x", nil), scriptDoc("b", "y", nil)), nil)
	var ids []string
	for _, d := range res.Returned.Docs() {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, ",") != "a.1,a.2,b.1,b.2" {
		t.Fatalf("ids = %v", ids)
	}
	res, _ = e.Run(context.Background(), scriptFlow(t, `{"script":"drop"}`), docs.NewSet(scriptDoc("a", "x", nil)), nil)
	if res.Returned.Len() != 0 || res.Failed.Len() != 0 || res.Trace[1].Dropped != 1 {
		t.Fatalf("drop: returned=%d", res.Returned.Len())
	}
}

func TestScriptStepConfigurationErrors(t *testing.T) {
	e := scriptEngine(t)
	for _, cfg := range []string{`{"script":"nope"}`, `{"script":"junk"}`} {
		res, _ := e.Run(context.Background(), scriptFlow(t, cfg), docs.NewSet(scriptDoc("a", "x", nil)), nil)
		if res.Failed.Len() != 1 || !strings.HasPrefix(res.Failed.At(0).Err().Code, "script.") {
			t.Errorf("%s: %+v", cfg, res.Failed.At(0).Err())
		}
	}
}

func TestScriptStepManyConcurrentDocuments(t *testing.T) {
	e := scriptEngine(t)
	in := docs.NewSet()
	for i := 0; i < 300; i++ {
		in.Add(scriptDoc(fmt.Sprintf("d%d", i), fmt.Sprintf("payload-%d", i), nil))
	}
	res, err := e.Run(context.Background(), scriptFlow(t, `{"script":"upper"}`), in, nil)
	if err != nil || res.Returned.Len() != 300 {
		t.Fatalf("err=%v returned=%d", err, res.Returned.Len())
	}
	for i, d := range res.Returned.Docs() {
		if want := strings.ToUpper(fmt.Sprintf("payload-%d", i)); scriptBody(t, d) != want {
			t.Fatalf("doc %d = %q want %q", i, scriptBody(t, d), want)
		}
	}
}

func TestScriptStepLargePayloadSpills(t *testing.T) {
	e := scriptEngine(t)
	big := strings.Repeat("abcdefgh", 1<<20) // 8 MiB, above the default spool threshold
	p, err := e.Spool.FromBytes([]byte(big))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Run(context.Background(), scriptFlow(t, `{"script":"upper","memoryMiB":64}`), docs.NewSet(docs.New("big", nil, p)), nil)
	if err != nil || res.Returned.Len() != 1 {
		t.Fatalf("err=%v returned=%d failed=%d", err, res.Returned.Len(), res.Failed.Len())
	}
	out := res.Returned.At(0)
	if out.Payload().Size() != int64(len(big)) {
		t.Fatalf("size %d", out.Payload().Size())
	}
	got := scriptBody(t, out)
	if !strings.HasPrefix(got, "ABCDEFGH") || !strings.HasSuffix(got, "ABCDEFGH") {
		t.Fatal("content mismatch")
	}
}

func TestScriptStepSchemaValidates(t *testing.T) {
	_, diags, err := flow.Parse([]byte(mk("f", []string{
		"start:start:" + startCfg, "s:script:" + `{"script":"x","capabilities":["network"]}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "s", "s:default": "r"})))
	if err == nil && len(diags) == 0 {
		t.Fatal("unknown capability should be a schema error")
	}
}

func BenchmarkScriptStep(b *testing.B) {
	we := wasm.NewEngine(wasm.EngineOptions{})
	defer func() { _ = we.Close(context.Background()) }()
	mods := engine.NewScriptModules()
	mods.Register("upper", wasmtest.Upper())
	e := engine.New()
	e.Registry.RegisterScriptRunner(engine.NewScriptRunner(we, mods))
	f, _, err := flow.Parse([]byte(mk("f", []string{
		"start:start:" + startCfg, "s:script:" + `{"script":"upper"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "s", "s:default": "r"})))
	if err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{1, 100} {
		b.Run(fmt.Sprintf("docs=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				in := docs.NewSet()
				for j := 0; j < n; j++ {
					in.Add(scriptDoc(fmt.Sprint(j), "some payload of modest size", nil))
				}
				res, err := e.Run(context.Background(), f, in, nil)
				if err != nil || res.Failed.Len() != 0 {
					b.Fatal(err)
				}
				_ = res.Returned.Close()
			}
		})
	}
}
