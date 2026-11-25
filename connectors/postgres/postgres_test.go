// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestBindPositional(t *testing.T) {
	sql, args, err := ParseStatement([]byte(`{"sql":"select $1, $2, $3, $4","params":[1, 2.5, "x'; drop table t;--", null]}`))
	if err != nil {
		t.Fatal(err)
	}
	if sql != "select $1, $2, $3, $4" || len(args) != 4 {
		t.Fatalf("got %q %v", sql, args)
	}
	if args[0] != int64(1) || args[1] != 2.5 || args[2] != "x'; drop table t;--" || args[3] != nil {
		t.Fatalf("bad args: %#v", args)
	}
}

func TestBindNamedAndJSON(t *testing.T) {
	args, err := BindParams(json.RawMessage(`{"id": 9007199254740993, "doc": {"a":[1]}}`))
	if err != nil {
		t.Fatal(err)
	}
	na := args[0].(pgx.NamedArgs)
	if na["id"] != int64(9007199254740993) {
		t.Fatalf("lost precision: %#v", na["id"])
	}
	if string(na["doc"].(json.RawMessage)) != `{"a":[1]}` {
		t.Fatalf("doc: %#v", na["doc"])
	}
}

func TestBindErrors(t *testing.T) {
	for _, in := range []string{`not json`, `{"sql":""}`, `{"sql":"select 1","params":5}`, `{"sql":"select 1","params":"x"}`} {
		_, _, err := ParseStatement([]byte(in))
		if !errors.Is(err, errInput) {
			t.Errorf("%s: want errInput, got %v", in, err)
		}
	}
	if a, err := BindParams(nil); a != nil || err != nil {
		t.Errorf("nil params: %v %v", a, err)
	}
}

func TestRowToDocument(t *testing.T) {
	uuid := [16]byte{0xde, 0xad, 0xbe, 0xef, 0, 1, 0, 2, 0, 3, 0, 4, 0, 5, 0, 6}
	b, err := RowToDocument([]string{"id", "name", "n", "raw", "u"}, []any{int64(1), "a\"b", nil, []byte("hi"), uuid})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not json: %s", b)
	}
	if m["id"] != float64(1) || m["name"] != `a"b` || m["n"] != nil || m["raw"] != "aGk=" || m["u"] != "deadbeef-0001-0002-0003-000400050006" {
		t.Fatalf("got %s", b)
	}
}

func TestMapError(t *testing.T) {
	cases := map[string]connectorv1.DocumentStatus{
		"23505": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
		"42601": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
		"22P02": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
		"08006": connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
		"57P01": connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
		"40P01": connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
	}
	for code, want := range cases {
		got, msg := MapError(&pgconn.PgError{Code: code, Message: "boom"})
		if got != want || !strings.Contains(msg, code) {
			t.Errorf("%s: got %v %q", code, got, msg)
		}
	}
	if st, _ := MapError(errors.New("dial tcp: refused")); st != connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE {
		t.Error("generic error must be FAILURE")
	}
	if _, msg := MapError(errors.New("auth failed for s3cret"), "s3cret"); strings.Contains(msg, "s3cret") {
		t.Error("secret leaked")
	}
}

func settings(t testing.TB, m map[string]any) *connectorv1.ConnectionConfig {
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return &connectorv1.ConnectionConfig{Settings: s}
}

func TestConfig(t *testing.T) {
	cfg, err := ParseConfig(settings(t, map[string]any{"host": "h", "database": "d", "user": "u@x", "password": "p@ss/w:rd?#"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 5432 || cfg.SSLMode != "require" || cfg.PoolMaxConns != 4 {
		t.Fatalf("defaults: %+v", cfg)
	}
	u, err := url.Parse(cfg.URL())
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw != "p@ss/w:rd?#" || u.User.Username() != "u@x" || u.Host != "h:5432" {
		t.Fatalf("url round trip failed: %s", u.Redacted())
	}
	if _, err := cfg.poolConfig(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]any{
		{"database": "d", "user": "u"},
		{"host": "h", "user": "u"},
		{"host": "h", "database": "d", "user": "u", "sslMode": "allow"},
		{"host": "h", "database": "d", "user": "u", "poolMaxConns": float64(0)},
		{"host": "h", "database": "d", "user": "u", "port": float64(70000)},
	} {
		if _, err := ParseConfig(settings(t, bad)); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestTestRedactsPassword(t *testing.T) {
	c := New()
	defer c.Close()
	resp, err := c.Test(t.Context(), &connectorv1.TestRequest{Connection: settings(t, map[string]any{
		"host": "127.0.0.1", "port": float64(1), "database": "d", "user": "u", "password": "hunter2-secret",
		"sslMode": "disable", "connectTimeoutSeconds": float64(2),
	})})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Ok || strings.Contains(resp.Diagnostic, "hunter2-secret") {
		t.Fatalf("unexpected: %+v", resp)
	}
}

// liveSettings returns settings for a live database from NURTUO_TEST_POSTGRES_* env vars, or
// skips. NURTUO_TEST_POSTGRES_URL is a postgres:// URL (e.g. from a docker compose service).
func liveSettings(t testing.TB) (map[string]any, string) {
	raw := os.Getenv("NURTUO_TEST_POSTGRES_URL")
	if raw == "" {
		t.Skip("NURTUO_TEST_POSTGRES_URL not set; skipping tests that need a live PostgreSQL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("NURTUO_TEST_POSTGRES_URL: %v", err)
	}
	port := 5432
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	pw, _ := u.User.Password()
	ssl := u.Query().Get("sslmode")
	if ssl == "" {
		ssl = "disable"
	}
	return map[string]any{
		"host": u.Hostname(), "port": float64(port), "database": strings.TrimPrefix(u.Path, "/"),
		"user": u.User.Username(), "password": pw, "sslMode": ssl,
	}, pw
}

func doc(id, js string) contracttest.Document { return contracttest.Document{ID: id, Data: []byte(js)} }

func TestContract(t *testing.T) {
	conn := settings(t, map[string]any{"host": "127.0.0.1", "port": float64(1), "database": "d", "user": "u", "sslMode": "disable", "connectTimeoutSeconds": float64(2)})
	var secrets []string
	var cases []contracttest.ExecuteCase
	if os.Getenv("NURTUO_TEST_POSTGRES_URL") != "" {
		s, pw := liveSettings(t)
		conn, secrets = settings(t, s), []string{pw}
		const ok, appErr = connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS, connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
		q := &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_QUERY}
		cases = []contracttest.ExecuteCase{
			{Name: "query", Start: q, Documents: []contracttest.Document{
				doc("a", `{"sql":"select $1::int as n","params":[7]}`),
				doc("bad-sql", `{"sql":"selec 1"}`),
				doc("bad-input", `nope`),
			}, Want: map[string]connectorv1.DocumentStatus{"a": ok, "bad-sql": appErr, "bad-input": appErr}},
		}
	}
	contracttest.Run(t, contracttest.Options{
		New:        func(t testing.TB) connector.Connector { c := New(); t.Cleanup(c.Close); return c },
		Connection: conn,
		Secrets:    secrets,
		Execute:    cases,
	})
}

func TestLiveRoundTrip(t *testing.T) {
	s, _ := liveSettings(t)
	conn := settings(t, s)
	c := New()
	defer c.Close()
	ctx := t.Context()
	p, _, release, err := c.pool(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, _ = p.Exec(ctx, "drop table if exists nurtuo_conn_test")
	if _, err := p.Exec(ctx, "create table nurtuo_conn_test (id int primary key, name text)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = p.Exec(ctx, "drop table if exists nurtuo_conn_test") }()

	run := func(op connectorv1.OperationType, tx bool, docs ...string) map[string]*connectorv1.DocumentResult {
		o, _ := structpb.NewStruct(map[string]any{"transaction": tx})
		return runBatch(t, c, &connectorv1.ExecuteStart{Connection: conn, OperationType: op, Options: o}, docs)
	}
	ins := connectorv1.OperationType_OPERATION_TYPE_CREATE
	res := run(ins, true,
		`{"sql":"insert into nurtuo_conn_test values (@id, @name)","params":{"id":1,"name":"a"}}`,
		`{"sql":"insert into nurtuo_conn_test values ($1, $2)","params":[1,"dup"]}`)
	if res["0"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE || res["1"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR {
		t.Fatalf("tx rollback statuses: %v", res)
	}
	var n int
	if err := p.QueryRow(ctx, "select count(*) from nurtuo_conn_test").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollback failed: n=%d err=%v", n, err)
	}
	res = run(ins, false, `{"sql":"insert into nurtuo_conn_test values ($1, $2)","params":[1,"a"]}`)
	if res["0"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS || res["0"].Tracked["rows_affected"] != "1" {
		t.Fatalf("insert: %v", res)
	}
	res = run(connectorv1.OperationType_OPERATION_TYPE_QUERY, false, `{"sql":"select * from nurtuo_conn_test where id=$1","params":[1]}`)
	if r := res["0"]; r.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS || len(r.OutputDocumentIds) != 1 {
		t.Fatalf("query: %v", res)
	}

	// QUERY is READ ONLY (enforced by the database): a write is rejected with APPLICATION_ERROR,
	// with and without the transaction option, and nothing is written.
	for _, tx := range []bool{false, true} {
		res = run(connectorv1.OperationType_OPERATION_TYPE_QUERY, tx, `{"sql":"insert into nurtuo_conn_test values (2, 'x')"}`)
		r := res["0"]
		if r.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR || !strings.Contains(r.Message, "25006") {
			t.Fatalf("INSERT in QUERY (tx=%v) must be rejected, got %v", tx, res)
		}
	}
	if err := p.QueryRow(ctx, "select count(*) from nurtuo_conn_test").Scan(&n); err != nil || n != 1 {
		t.Fatalf("QUERY wrote data: n=%d err=%v", n, err)
	}
	// Reject transaction control in a QUERY batch even with the table owner's credentials.
	res = run(connectorv1.OperationType_OPERATION_TYPE_QUERY, true,
		`{"sql":"select * from nurtuo_conn_test"}`, `{"sql":"commit"}`)
	if res["0"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE ||
		len(res["0"].OutputDocumentIds) != 0 ||
		res["1"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR ||
		res["1"].Message != "QUERY cannot change transaction or session settings" {
		t.Fatalf("transaction control in QUERY batch: %v", res)
	}
	res = run(connectorv1.OperationType_OPERATION_TYPE_QUERY, true,
		`{"sql":"select * from nurtuo_conn_test"}`,
		`{"sql":"insert into nurtuo_conn_test values (2, 'x')"}`)
	if res["0"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE ||
		len(res["0"].OutputDocumentIds) != 0 ||
		res["1"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR ||
		!strings.Contains(res["1"].Message, "25006") {
		t.Fatalf("write in read-only QUERY batch: %v", res)
	}
	// An ordinary batch still returns all results after the transaction completes.
	res = run(connectorv1.OperationType_OPERATION_TYPE_QUERY, true,
		`{"sql":"select * from nurtuo_conn_test"}`, `{"sql":"select 2"}`)
	for _, r := range res {
		if r.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS || len(r.OutputDocumentIds) != 1 {
			t.Fatalf("read-only QUERY batch: %v", res)
		}
	}
	// the other operations keep working
	res = run(connectorv1.OperationType_OPERATION_TYPE_UPDATE, false, `{"sql":"update nurtuo_conn_test set name='b' where id=1"}`)
	if res["0"].Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
		t.Fatalf("UPDATE must still work: %v", res)
	}
}

func TestLiveQueryBufferBudget(t *testing.T) {
	s, _ := liveSettings(t)
	conn := settings(t, s)
	const row = `{"n":"0123456789"}`
	c := New()
	c.MaxQueryResultBytes = len(row)
	defer c.Close()

	const query = `{"sql":"select '0123456789' as n"}`
	run := func(transaction bool) map[string]*connectorv1.DocumentResult {
		return runBatch(t, c, &connectorv1.ExecuteStart{
			Connection: conn, OperationType: connectorv1.OperationType_OPERATION_TYPE_QUERY,
			Options: &structpb.Struct{Fields: map[string]*structpb.Value{"transaction": structpb.NewBoolValue(transaction)}},
		}, []string{query, query})
	}
	for _, r := range run(false) {
		if r.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS || len(r.OutputDocumentIds) != 1 {
			t.Fatalf("per-document buffer should admit one row at the limit: %v", r)
		}
	}
	results := run(true)
	first, second := results["0"], results["1"]
	if first.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE ||
		second.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR ||
		!strings.Contains(second.Message, "buffer limit") ||
		len(first.OutputDocumentIds)+len(second.OutputDocumentIds) != 0 {
		t.Fatalf("batch should exceed the shared buffer and return no rows: %v", results)
	}
}
