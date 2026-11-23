// SPDX-License-Identifier: AGPL-3.0-only

package connhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

const token = "tok-SUPER-secret-9876"

// fake is a connector that records its Test calls and Close, and can echo the token in its
// diagnostic to prove the manager redacts it.
type fake struct {
	tests    atomic.Int32
	closed   atomic.Bool
	testOK   bool
	leak     bool
	notImpl  bool
	lastConf *connectorv1.ConnectionConfig
}

func (f *fake) Describe(context.Context) (*connectorv1.ConnectorDescriptor, error) {
	schema, _ := structpb.NewStruct(map[string]any{"properties": map[string]any{
		"auth": map[string]any{"properties": map[string]any{"token": map[string]any{"x-nurtuo-secret": true}}},
	}})
	return &connectorv1.ConnectorDescriptor{Id: "fake", ConnectionSchema: schema}, nil
}
func (f *fake) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}
func (f *fake) Execute(connectorv1.ConnectorService_ExecuteServer) error { return nil }
func (f *fake) Test(_ context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	f.tests.Add(1)
	f.lastConf = req.GetConnection()
	if f.notImpl {
		return nil, connector.ErrNotImplemented
	}
	diag := "rejected"
	if f.leak {
		diag = "dial failed with Bearer " + req.GetConnection().GetSettings().AsMap()["auth"].(map[string]any)["token"].(string)
	}
	return &connectorv1.TestResponse{Ok: f.testOK, Diagnostic: diag}, nil
}
func (f *fake) Close() { f.closed.Store(true) }

type prov map[string]string

func (p prov) Get(_ context.Context, n string) (secrets.Secret, error) {
	if v, ok := p[n]; ok {
		return secrets.New(v), nil
	}
	return secrets.Secret{}, fmt.Errorf("secret %q: %w", n, secrets.ErrNotFound)
}

type opts struct{ testOK, leak, notImpl bool }

type fixture struct {
	m     *Manager
	made  []*fake
	proto opts
}

func newFixture(mode string, proto opts) *fixture {
	fx := &fixture{proto: proto}
	reg := NewRegistry()
	reg.Register("fake", func() connector.Connector {
		f := &fake{testOK: fx.proto.testOK, leak: fx.proto.leak, notImpl: fx.proto.notImpl}
		fx.made = append(fx.made, f)
		return f
	})
	red := secrets.NewRedactor()
	fx.m = NewManager(reg, &connections.Resolver{Secrets: prov{"tok": token}, Redactor: red})
	fx.m.TestMode = mode
	return fx
}

func def(t *testing.T, id, conn, tokenVal string) *connections.Definition {
	t.Helper()
	d, err := connections.Parse([]byte(fmt.Sprintf(`{"schema":"nurtuo.connection/v1","id":%q,"connector":%q,"settings":{"auth":{"token":%q},"host":"h"}}`, id, conn, tokenVal)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func fl(id string, steps ...string) *flow.Flow {
	f := &flow.Flow{ID: id}
	for i, c := range steps {
		f.Steps = append(f.Steps, flow.Step{ID: fmt.Sprintf("s%d", i), Kind: flow.KindConnector, Config: json.RawMessage(c)})
	}
	return f
}

const stepDefault = `{"connector":"fake","operation":"GET"}`

func TestPrepareCommitLookupDescribeClose(t *testing.T) {
	fx := newFixture(TestStrict, opts{testOK: true})
	p, err := fx.m.Prepare(context.Background(), "dep", []*flow.Flow{fl("f1", stepDefault, `{"connector":"fake","operation":"GET","connection":"erp"}`)},
		[]*connections.Definition{def(t, "erp", "fake", "secret://tok"), def(t, "unused", "other", "secret://missing")})
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.made) != 1 {
		t.Fatalf("only referenced connections are instantiated, made %d", len(fx.made))
	}
	if fx.made[0].tests.Load() != 1 {
		t.Fatal("connection not tested on deploy")
	}
	if got := fx.made[0].lastConf.GetSettings().AsMap()["auth"].(map[string]any)["token"]; got != token {
		t.Fatalf("connector must receive the resolved secret, got %v", got)
	}
	if s := p.Summary(); s != "connections: erp ok" {
		t.Fatalf("summary %q", s)
	}
	// Not visible before Commit.
	if _, err := fx.m.Lookup("f1", flow.ConnectorConfig{Connector: "fake"}); err == nil {
		t.Fatal("lookup before commit must fail")
	}
	fx.m.Commit(p)
	for _, c := range []flow.ConnectorConfig{{Connector: "fake"}, {Connector: "fake", Connection: "erp"}} {
		e, err := fx.m.Lookup("f1", c)
		if err != nil || e.Connection == nil {
			t.Fatalf("lookup %+v: %v", c, err)
		}
	}
	if _, err := fx.m.Lookup("f1", flow.ConnectorConfig{Connector: "fake", Connection: "nope"}); err == nil {
		t.Fatal("unknown connection")
	}
	if _, err := fx.m.Lookup("other-flow", flow.ConnectorConfig{Connector: "fake"}); err == nil {
		t.Fatal("flow of another deployment must not see these connections")
	}

	info, _ := json.Marshal(fx.m.Describe("dep"))
	if strings.Contains(string(info), token) || !strings.Contains(string(info), "secret://tok") {
		t.Fatalf("Describe leaked or lost the reference: %s", info)
	}

	// Recommit closes the old instances; Remove closes the current ones.
	p2, err := fx.m.Prepare(context.Background(), "dep", []*flow.Flow{fl("f1", stepDefault)}, []*connections.Definition{def(t, "erp", "fake", "secret://tok")})
	if err != nil {
		t.Fatal(err)
	}
	if fx.made[0].closed.Load() {
		t.Fatal("old instance closed before the new set was committed")
	}
	fx.m.Commit(p2)
	if !fx.made[0].closed.Load() {
		t.Fatal("old instance not closed on recommit")
	}
	fx.m.Remove("dep")
	if !fx.made[1].closed.Load() {
		t.Fatal("instance not closed on undeploy")
	}
	if _, err := fx.m.Lookup("f1", flow.ConnectorConfig{Connector: "fake"}); err == nil {
		t.Fatal("lookup after undeploy")
	}
}

func TestDescribeMasksLiteralSecretFields(t *testing.T) {
	fx := newFixture(TestOff, opts{})
	// Bypass CheckLiterals by mutating after Parse to simulate a connector without schema marks.
	d := def(t, "erp", "fake", "secret://tok")
	p, err := fx.m.Prepare(context.Background(), "dep", []*flow.Flow{fl("f", stepDefault)}, []*connections.Definition{d})
	if err != nil {
		t.Fatal(err)
	}
	fx.m.Commit(p)
	d.Settings["auth"].(map[string]any)["token"] = "literal-should-be-masked"
	b, _ := json.Marshal(fx.m.Describe("dep"))
	if strings.Contains(string(b), "literal-should-be-masked") {
		t.Fatalf("literal at a secret path leaked: %s", b)
	}
}

func TestReferenceErrors(t *testing.T) {
	fx := newFixture(TestStrict, opts{testOK: true})
	ctx := context.Background()
	two := []*connections.Definition{def(t, "a", "fake", "secret://tok"), def(t, "b", "fake", "secret://tok")}
	for name, tc := range map[string]struct {
		flows []*flow.Flow
		defs  []*connections.Definition
		want  string
	}{
		"no connection":     {[]*flow.Flow{fl("f", stepDefault)}, nil, "defines none"},
		"ambiguous default": {[]*flow.Flow{fl("f", stepDefault)}, two, "several connections"},
		"undefined":         {[]*flow.Flow{fl("f", `{"connector":"fake","operation":"GET","connection":"zzz"}`)}, two, `"zzz", which is not defined`},
		"wrong connector":   {[]*flow.Flow{fl("f", `{"connector":"http","operation":"GET","connection":"a"}`)}, two, `is for connector "fake"`},
		"duplicate def":     {[]*flow.Flow{fl("f", stepDefault)}, []*connections.Definition{two[0], two[0]}, "more than once"},
		"unregistered":      {[]*flow.Flow{fl("f", `{"connector":"zzz","operation":"GET","connection":"c"}`)}, []*connections.Definition{def(t, "c", "zzz", "x")}, `"zzz" is not registered`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fx.m.Prepare(ctx, "dep", tc.flows, tc.defs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestMissingSecretFailsAndClosesEarlierInstances(t *testing.T) {
	fx := newFixture(TestStrict, opts{testOK: true})
	_, err := fx.m.Prepare(context.Background(), "dep",
		[]*flow.Flow{fl("f", `{"connector":"fake","operation":"GET","connection":"a"}`, `{"connector":"fake","operation":"GET","connection":"b"}`)},
		[]*connections.Definition{def(t, "a", "fake", "secret://tok"), def(t, "b", "fake", "secret://gone")})
	if err == nil || !strings.Contains(err.Error(), `"gone"`) || !strings.Contains(err.Error(), `"b"`) || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
	for i, f := range fx.made {
		if !f.closed.Load() {
			t.Fatalf("instance %d leaked after failed prepare", i)
		}
	}
}

func TestLiteralInSecretFieldRejected(t *testing.T) {
	fx := newFixture(TestStrict, opts{testOK: true})
	_, err := fx.m.Prepare(context.Background(), "dep", []*flow.Flow{fl("f", stepDefault)}, []*connections.Definition{def(t, "a", "fake", "plain-literal-pw")})
	if err == nil || !strings.Contains(err.Error(), "auth.token") || strings.Contains(err.Error(), "plain-literal-pw") {
		t.Fatalf("err = %v", err)
	}
}

func TestTestModes(t *testing.T) {
	ctx := context.Background()
	flows := []*flow.Flow{fl("f", stepDefault)}
	defs := func() []*connections.Definition {
		return []*connections.Definition{def(t, "a", "fake", "secret://tok")}
	}

	// strict + failing test + connector leaking the token in its diagnostic: error, redacted.
	fx := newFixture(TestStrict, opts{testOK: false, leak: true})
	_, err := fx.m.Prepare(ctx, "d", flows, defs())
	if err == nil || !strings.Contains(err.Error(), "test failed") || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("strict: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked through the test diagnostic: %v", err)
	}
	if !strings.Contains(err.Error(), "***") || !fx.made[0].closed.Load() {
		t.Fatalf("expected redacted diag and closed instance: %v", err)
	}

	// warn: deploys, summary carries the (redacted) warning.
	fx = newFixture(TestWarn, opts{testOK: false, leak: true})
	p, err := fx.m.Prepare(ctx, "d", flows, defs())
	if err != nil {
		t.Fatalf("warn: %v", err)
	}
	if s := p.Summary(); !strings.Contains(s, "warning") || strings.Contains(s, token) {
		t.Fatalf("summary %q", s)
	}

	// off: Test is not called.
	fx = newFixture(TestOff, opts{testOK: false})
	if _, err := fx.m.Prepare(ctx, "d", flows, defs()); err != nil || fx.made[0].tests.Load() != 0 {
		t.Fatalf("off: err=%v tests=%d", err, fx.made[0].tests.Load())
	}

	// ErrNotImplemented: skipped, not an error.
	fx = newFixture(TestStrict, opts{notImpl: true})
	p, err = fx.m.Prepare(ctx, "d", flows, defs())
	if err != nil || !strings.Contains(p.Summary(), "not testable") {
		t.Fatalf("not implemented: %v %v", err, p)
	}
}

func TestRedactingWrapper(t *testing.T) {
	red := secrets.NewRedactor()
	red.Add(token)
	w := &redacting{c: &leaky{}, r: red}
	s := &sink{}
	err := w.Execute(s)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("execute error not redacted: %v", err)
	}
	if len(s.sent) != 1 || strings.Contains(s.sent[0].GetResult().GetMessage(), token) {
		t.Fatalf("result message not redacted: %v", s.sent)
	}
}

type leaky struct{ fake }

func (l *leaky) Execute(s connectorv1.ConnectorService_ExecuteServer) error {
	_ = s.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: &connectorv1.DocumentResult{DocumentId: "d", Message: "401 for " + token}}})
	return fmt.Errorf("auth failed with %s", token)
}

type sink struct {
	connectorv1.ConnectorService_ExecuteServer
	sent []*connectorv1.ExecuteResponse
}

func (s *sink) Send(m *connectorv1.ExecuteResponse) error { s.sent = append(s.sent, m); return nil }

func TestBuiltins(t *testing.T) {
	r := Builtins()
	ids := r.IDs()
	if strings.Join(ids, ",") != "http,nats,postgres,sftp" {
		t.Fatalf("ids %v", ids)
	}
	for _, id := range ids {
		c, err := r.New(id)
		if err != nil {
			t.Fatal(err)
		}
		d, err := c.Describe(context.Background())
		if err != nil || d.GetId() != id {
			t.Fatalf("%s: %v %v", id, d.GetId(), err)
		}
		// Built-ins must declare which settings are secret so literals are rejected.
		if len(connections.SecretPaths(d.GetConnectionSchema())) == 0 {
			t.Errorf("%s declares no secret fields in its connection schema", id)
		}
	}
	if _, err := r.New("nope"); err == nil || !strings.Contains(err.Error(), "available: http") {
		t.Fatalf("%v", err)
	}
}
