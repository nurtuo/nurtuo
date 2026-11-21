// SPDX-License-Identifier: AGPL-3.0-only

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

func mustFlow(t *testing.T, raw string) *flow.Flow {
	t.Helper()
	f, diags, err := flow.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("flow invalid: %v %v", err, diags)
	}
	return f
}

// mk builds a flow JSON from steps ("id:kind:configJSON") and edges.
func mk(id string, steps []string, edges map[string]string) string {
	var ss []string
	for _, s := range steps {
		p := strings.SplitN(s, ":", 3)
		ss = append(ss, fmt.Sprintf(`{"id":%q,"kind":%q,"config":%s}`, p[0], p[1], p[2]))
	}
	var es []string
	for k, v := range edges {
		es = append(es, fmt.Sprintf("%q:%q", k, v))
	}
	return fmt.Sprintf(`{"schema":"nurtuo.flow/v1","id":%q,"name":"N","revision":1,"steps":[%s],"edges":{%s}}`,
		id, strings.Join(ss, ","), strings.Join(es, ","))
}

const startCfg = `{"type":"manual"}`

func set(props ...map[string]string) *docs.Set {
	s := docs.NewSet()
	for i, p := range props {
		s.Add(docs.New(fmt.Sprintf("d%d", i+1), p, nil))
	}
	return s
}

func ids(s *docs.Set) []string {
	var out []string
	for _, d := range s.Docs() {
		out = append(out, d.ID)
	}
	return out
}

func trace(r *engine.Result) []string {
	var out []string
	for _, t := range r.Trace {
		out = append(out, t.StepID)
	}
	return out
}

func TestDecisionSplitsSet(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg,
		"dec:decision:" + `{"cel":"doc.props.kind == 'a'"}`,
		"tag:setProperties:" + `{"properties":[{"name":"path","value":"true-{{doc.id}}"}]}`,
		"ret:returnDocuments:{}",
	}, map[string]string{"start:default": "dec", "dec:true": "tag", "dec:false": "ret", "tag:default": "ret"}))
	e := engine.New()
	res, err := e.Run(context.Background(), f, set(
		map[string]string{"kind": "a"}, map[string]string{"kind": "b"}, map[string]string{"kind": "a"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	// true port first (d1,d3), then false (d2); the flow merges at ret, so ret runs twice.
	if got, want := ids(res.Returned), []string{"d1", "d3", "d2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("returned %v want %v", got, want)
	}
	if res.Returned.At(0).Props["path"] != "true-d1" || res.Returned.At(2).Props["path"] != "" {
		t.Fatalf("props wrong: %v %v", res.Returned.At(0).Props, res.Returned.At(2).Props)
	}
	if got, want := trace(res), []string{"start", "dec", "tag", "ret", "ret"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trace %v want %v", got, want)
	}
	if res.Trace[1].In != 3 || res.Trace[1].Out != 3 {
		t.Fatalf("decision trace %+v", res.Trace[1])
	}
}

func TestDecisionConditionErrorFailsDocument(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "dec:decision:" + `{"cel":"bogus()"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "dec", "dec:true": "r", "dec:false": "r"}))
	res, err := engine.New().Run(context.Background(), f, set(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Returned.Len() != 0 || res.Failed.Len() != 1 || res.Failed.At(0).Status() != docs.StatusError {
		t.Fatalf("returned=%d failed=%d", res.Returned.Len(), res.Failed.Len())
	}
}

func TestBranchRunsPathsInOrder(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg,
		"br:branch:" + `{"paths":3}`,
		"m1:map:" + `{"jsonata":"1"}`, "m2:map:" + `{"jsonata":"2"}`, "m3:map:" + `{"jsonata":"3"}`,
		"r1:returnDocuments:{}", "r2:returnDocuments:{}", "r3:returnDocuments:{}",
	}, map[string]string{
		"start:default": "br", "br:1": "m1", "br:2": "m2", "br:3": "m3",
		"m1:default": "r1", "m2:default": "r2", "m3:default": "r3",
	}))
	e := engine.New()
	var mu sync.Mutex
	var order []string
	e.Registry.Register(flow.KindMap, engine.PerDocument(func(_ context.Context, sc *engine.StepContext, d *docs.Document) (*docs.Document, error) {
		mu.Lock()
		order = append(order, sc.Step.ID+"/"+d.ID)
		mu.Unlock()
		d.Props["last"] = sc.Step.ID
		return d, nil
	}))
	res, err := e.Run(context.Background(), f, set(nil, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := trace(res), []string{"start", "br", "m1", "r1", "m2", "r2", "m3", "r3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trace %v want %v", got, want)
	}
	if res.Returned.Len() != 6 {
		t.Fatalf("returned %d", res.Returned.Len())
	}
	var lasts []string
	for _, d := range res.Returned.Docs() {
		lasts = append(lasts, d.Props["last"])
	}
	if want := []string{"m1", "m1", "m2", "m2", "m3", "m3"}; !reflect.DeepEqual(lasts, want) {
		t.Fatalf("lasts %v want %v", lasts, want)
	}
}

func TestRouteFirstMatchThenDefault(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg,
		"rt:route:" + `{"routes":[{"id":"big","cel":"doc.props.n >= 10"},{"id":"mid","cel":"doc.props.n >= 5"}]}`,
		"rb:returnDocuments:{}", "rm:stop:{}", "rd:returnDocuments:{}",
	}, map[string]string{"start:default": "rt", "rt:big": "rb", "rt:mid": "rd", "rt:default": "rd"}))
	res, err := engine.New().Run(context.Background(), f, set(
		map[string]string{"n": "12"}, map[string]string{"n": "7"}, map[string]string{"n": "1"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(res.Returned), []string{"d1", "d2", "d3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("returned %v want %v", got, want)
	}
	if got, want := trace(res), []string{"start", "rt", "rb", "rd", "rd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trace %v want %v", got, want)
	}
}

func failIfProp(attempts *sync.Map) engine.StepRunner {
	return engine.PerDocument(func(_ context.Context, sc *engine.StepContext, d *docs.Document) (*docs.Document, error) {
		switch d.Props["fail"] {
		case "always":
			return nil, errors.New("boom " + d.ID)
		case "once":
			n, _ := attempts.LoadOrStore(d.ID, new(int))
			*(n.(*int))++
			if *(n.(*int)) == 1 {
				return nil, errors.New("flaky " + d.ID)
			}
		}
		d.Props["connected"] = "yes"
		return d, nil
	})
}

func tryCatchFlow(t *testing.T, retries int) *flow.Flow {
	return mustFlow(t, mk("f", []string{
		"start:start:" + startCfg,
		"tc:tryCatch:" + fmt.Sprintf(`{"retries":%d}`, retries),
		"conn:connector:" + `{"connector":"x","operation":"GET"}`,
		"ok:returnDocuments:{}",
		"mark:setProperties:" + `{"properties":[{"name":"caught","value":"yes"}]}`,
		"bad:returnDocuments:{}",
	}, map[string]string{"start:default": "tc", "tc:try": "conn", "conn:default": "ok", "tc:catch": "mark", "mark:default": "bad"}))
}

func TestTryCatchIsolatesFailingDocuments(t *testing.T) {
	e := engine.New()
	e.Registry.Register(flow.KindConnector, failIfProp(new(sync.Map)))
	res, err := e.Run(context.Background(), tryCatchFlow(t, 0), set(
		nil, map[string]string{"fail": "always"}, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed.Len() != 0 {
		t.Fatalf("nothing should escape the try/catch, got %d", res.Failed.Len())
	}
	if got, want := ids(res.Returned), []string{"d1", "d3", "d2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("returned %v want %v", got, want)
	}
	good, bad := res.Returned.At(0), res.Returned.At(2)
	if good.Props["connected"] != "yes" || good.Props["caught"] != "" {
		t.Fatalf("good doc props %v", good.Props)
	}
	if bad.Props["caught"] != "yes" || !strings.Contains(bad.Props["nurtuo.error.message"], "boom d2") ||
		bad.Props["nurtuo.error.step"] != "conn" || bad.Status() != docs.StatusOK {
		t.Fatalf("bad doc props %v status %v", bad.Props, bad.Status())
	}
}

func TestTryCatchRetries(t *testing.T) {
	cases := []struct {
		name       string
		retries    int
		wantCaught int
	}{{"no retries: flaky doc is caught", 0, 1}, {"one retry: flaky doc recovers", 1, 0}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := engine.New()
			e.Registry.Register(flow.KindConnector, failIfProp(new(sync.Map)))
			res, err := e.Run(context.Background(), tryCatchFlow(t, c.retries), set(map[string]string{"fail": "once"}, nil), nil)
			if err != nil {
				t.Fatal(err)
			}
			caught := 0
			for _, d := range res.Returned.Docs() {
				if d.Props["caught"] == "yes" {
					caught++
				}
			}
			if caught != c.wantCaught || res.Returned.Len() != 2 {
				t.Fatalf("caught=%d returned=%d", caught, res.Returned.Len())
			}
		})
	}
}

func TestUncaughtFailureCollectedInResult(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "c:connector:" + `{"connector":"x","operation":"GET"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "c", "c:default": "r"}))
	e := engine.New()
	e.Registry.Register(flow.KindConnector, failIfProp(new(sync.Map)))
	res, _ := e.Run(context.Background(), f, set(map[string]string{"fail": "always"}, nil), nil)
	if res.Failed.Len() != 1 || res.Returned.Len() != 1 || res.Trace[1].Failed != 1 {
		t.Fatalf("failed=%d returned=%d trace=%+v", res.Failed.Len(), res.Returned.Len(), res.Trace)
	}
}

func TestMissingRunnerFailsDocuments(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "c:connector:" + `{"connector":"x","operation":"GET"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "c", "c:default": "r"}))
	res, err := engine.New().Run(context.Background(), f, set(nil), nil)
	if err != nil || res.Failed.Len() != 1 || res.Failed.At(0).Err().Code != "no_runner" {
		t.Fatalf("err=%v failed=%d", err, res.Failed.Len())
	}
}

func TestProcessCall(t *testing.T) {
	child := mustFlow(t, mk("child", []string{
		"start:start:" + startCfg,
		"set:setProperties:" + `{"properties":[{"name":"child","value":"hi {{doc.id}}"},{"name":"seen","scope":"process","value":"1"}]}`,
		"c:connector:" + `{"connector":"x","operation":"GET"}`,
		"r:returnDocuments:{}",
	}, map[string]string{"start:default": "set", "set:default": "c", "c:default": "r"}))
	parent := mustFlow(t, mk("parent", []string{
		"start:start:" + startCfg,
		"tc:tryCatch:{}",
		"pc:processCall:" + `{"flow":"child"}`,
		"ok:returnDocuments:{}",
		"caught:setProperties:" + `{"properties":[{"name":"caught","value":"yes"}]}`,
		"rc:returnDocuments:{}",
	}, map[string]string{"start:default": "tc", "tc:try": "pc", "pc:default": "ok", "tc:catch": "caught", "caught:default": "rc"}))
	e := engine.New()
	e.Registry.Register(flow.KindConnector, failIfProp(new(sync.Map)))
	e.Resolver = engine.MapResolver{"child": child}
	proc := engine.NewProcess(nil)
	res, err := e.Run(context.Background(), parent, set(nil, map[string]string{"fail": "always"}), proc)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(res.Returned), []string{"d1", "d2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("returned %v want %v", got, want)
	}
	if d := res.Returned.At(0); d.Props["child"] != "hi d1" || d.Props["caught"] != "" {
		t.Fatalf("d1 %v", d.Props)
	}
	if d := res.Returned.At(1); d.Props["caught"] != "yes" || d.Props["nurtuo.error.step"] != "c" {
		t.Fatalf("d2 %v", d.Props)
	}
	if _, ok := proc.Get("seen"); ok {
		t.Fatal("child process props must not leak into parent")
	}
}

func TestProcessCallUnknownFlowAndRecursion(t *testing.T) {
	self := mustFlow(t, mk("a", []string{
		"start:start:" + startCfg, "pc:processCall:" + `{"flow":"b"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "pc", "pc:default": "r"}))
	other := mustFlow(t, mk("b", []string{
		"start:start:" + startCfg, "pc:processCall:" + `{"flow":"a"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "pc", "pc:default": "r"}))
	cases := []struct {
		name string
		res  engine.FlowResolver
		code string
	}{
		{"unknown flow", engine.MapResolver{}, "processCall.resolve"},
		{"no resolver", nil, "processCall.resolve"},
		{"cycle hits depth limit", engine.MapResolver{"a": self, "b": other}, "processCall.depth"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := engine.New()
			e.Resolver = c.res
			e.MaxDepth = 4
			res, err := e.Run(context.Background(), self, set(nil), nil)
			if err != nil || res.Failed.Len() != 1 || res.Failed.At(0).Err().Code != c.code {
				t.Fatalf("err=%v failed=%d", err, res.Failed.Len())
			}
		})
	}
}

func TestStopEndsExecution(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "br:branch:" + `{"paths":2}`,
		"s:stop:" + `{"status":418,"message":"teapot"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "br", "br:1": "s", "br:2": "r"}))
	res, err := engine.New().Run(context.Background(), f, set(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop == nil || res.Stop.Status != 418 || res.Stop.Message != "teapot" || res.Returned.Len() != 0 {
		t.Fatalf("stop=%+v returned=%d", res.Stop, res.Returned.Len())
	}
}

func TestContextCancellation(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "c:connector:" + `{"connector":"x","operation":"GET"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "c", "c:default": "r"}))
	ctx, cancel := context.WithCancel(context.Background())
	e := engine.New()
	e.Registry.Register(flow.KindConnector, engine.StepRunnerFunc(func(ctx context.Context, _ *engine.StepContext, in *docs.Set) (*docs.Set, error) {
		cancel()
		return in, ctx.Err()
	}))
	res, err := e.Run(ctx, f, set(nil, nil), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if res.Returned.Len() != 0 {
		t.Fatalf("returned %d after cancel", res.Returned.Len())
	}
}

func TestMessageReplacesPayload(t *testing.T) {
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "m:message:" + `{"template":"hello {{doc.props.who}}"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "m", "m:default": "r"}))
	res, err := engine.New().Run(context.Background(), f, set(map[string]string{"who": "world"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := res.Returned.At(0).Payload().Bytes()
	if err != nil || string(b) != "hello world" {
		t.Fatalf("payload %q err %v", b, err)
	}
}

// The example flow shipped in examples/flows (get-employee): connector and map plug in as runners.
func TestGetEmployeeExample(t *testing.T) {
	raw, err := os.ReadFile("../../../../examples/flows/get-employee.flow.json")
	if err != nil {
		t.Skipf("example not loadable: %v", err)
	}
	f := mustFlow(t, string(raw))
	e := engine.New()
	e.Registry.Register(flow.KindConnector, engine.PerDocument(func(_ context.Context, _ *engine.StepContext, d *docs.Document) (*docs.Document, error) {
		if d.Props["missing"] == "1" {
			d.ID = "" // connector returned no row
		}
		return d, nil
	}))
	e.Registry.Register(flow.KindMap, engine.PerDocument(func(_ context.Context, _ *engine.StepContext, d *docs.Document) (*docs.Document, error) {
		d.Props["mapped"] = "true"
		return d, nil
	}))
	cases := []struct {
		name     string
		in       []map[string]string
		returned int
		status   int
	}{
		{"found", []map[string]string{nil}, 1, 0},
		{"not found", []map[string]string{{"missing": "1"}}, 0, 404},
		{"mixed set: found returned, then 404", []map[string]string{nil, {"missing": "1"}}, 1, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := e.Run(context.Background(), f, set(c.in...), nil)
			if err != nil {
				t.Fatal(err)
			}
			st := 0
			if res.Stop != nil {
				st = res.Stop.Status
			}
			if res.Returned.Len() != c.returned || st != c.status {
				t.Fatalf("returned=%d stop=%d", res.Returned.Len(), st)
			}
			for _, d := range res.Returned.Docs() {
				if d.Props["mapped"] != "true" {
					t.Fatal("map did not run")
				}
			}
		})
	}
}

func TestBasicEvaluator(t *testing.T) {
	ev := engine.BasicEvaluator{}
	d := docs.New("x1", map[string]string{"n": "10", "s": "a b"}, nil)
	p := engine.NewProcess(map[string]string{"k": "v"})
	cases := []struct {
		expr string
		want bool
		err  bool
	}{
		{"true", true, false}, {"!true", false, false},
		{"doc.id == 'x1'", true, false}, {"doc.id != ''", true, false},
		{"doc.props.n > 9", true, false}, {"doc.props.n >= 11", false, false},
		{"doc.props.s == 'a b'", true, false}, {"process.props.k == 'v'", true, false},
		{"doc.props.missing == ''", true, false}, {"nope(", false, true},
	}
	for _, c := range cases {
		got, err := ev.Condition(c.expr, d, p)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%q: got %v err %v", c.expr, got, err)
		}
	}
	if s, err := ev.Template("{{doc.id}}-{{process.props.k}}", d, p); err != nil || s != "x1-v" {
		t.Errorf("template %q %v", s, err)
	}
}
