// SPDX-License-Identifier: Apache-2.0

package nats_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	natsconn "github.com/nurtuo/nurtuo/connectors/nats"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

func execDocs(t *testing.T, url string, start *connectorv1.ExecuteStart, docs ...*connectorv1.DocumentChunk) (map[string]*connectorv1.DocumentResult, error) {
	t.Helper()
	start.Connection = conn(t, map[string]any{"url": url})
	in := []*connectorv1.ExecuteRequest{{Msg: &connectorv1.ExecuteRequest_Start{Start: start}}}
	for _, d := range docs {
		in = append(in, &connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: d}})
	}
	in = append(in, &connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}})
	cl := &memStream{ctx: context.Background(), in: in}
	err := natsconn.New().Execute(cl)
	res := map[string]*connectorv1.DocumentResult{}
	for _, r := range cl.out {
		if x := r.GetResult(); x != nil {
			res[x.DocumentId] = x
		}
	}
	return res, err
}

func createStart() *connectorv1.ExecuteStart {
	return &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE}
}

func TestRejectsWildcardSubjectsAndBadHeaders(t *testing.T) {
	s := startServer(t, false)
	chunk := func(id string, props map[string]string) *connectorv1.DocumentChunk {
		return &connectorv1.DocumentChunk{DocumentId: id, Properties: props, Data: []byte("x"), Last: true}
	}
	res, err := execDocs(t, s.ClientURL(), createStart(),
		chunk("star", map[string]string{natsconn.PropSubject: "a.*"}),
		chunk("gt", map[string]string{natsconn.PropSubject: "a.>"}),
		chunk("crlfv", map[string]string{natsconn.PropSubject: "a.b", natsconn.PropHeaderPrefix + "X": "v\r\nEvil: 1"}),
		chunk("crlfn", map[string]string{natsconn.PropSubject: "a.b", natsconn.PropHeaderPrefix + "X\nY": "v"}),
		chunk("ok", map[string]string{natsconn.PropSubject: "a.b"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"star", "gt", "crlfv", "crlfn"} {
		if got := res[id].GetStatus(); got != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR {
			t.Errorf("%s: status %v (%s), want APPLICATION_ERROR", id, got, res[id].GetMessage())
		}
	}
	if got := res["ok"].GetStatus(); got != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
		t.Errorf("ok: %v %s", got, res["ok"].GetMessage())
	}
}

func TestJetStreamPublishDedupesByDocumentID(t *testing.T) {
	s := startServer(t, false)
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ORD", Subjects: []string{"ord.>"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		res, err := execDocs(t, s.ClientURL(), &connectorv1.ExecuteStart{
			OperationType: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: natsconn.CustomJetStreamPub,
		}, &connectorv1.DocumentChunk{DocumentId: "doc-1", Properties: map[string]string{natsconn.PropSubject: "ord.1"}, Data: []byte("x"), Last: true})
		if err != nil || res["doc-1"].GetStatus() != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
			t.Fatalf("publish %d: %v %v", i, err, res["doc-1"])
		}
	}
	info, err := st.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("stream has %d msgs after retry, want 1 (dedupe by Nats-Msg-Id)", info.State.Msgs)
	}
}

func TestTestHonoursContext(t *testing.T) {
	s := startServer(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	resp, _ := natsconn.New().Test(ctx, &connectorv1.TestRequest{Connection: conn(t, map[string]any{"url": s.ClientURL()})})
	if resp.GetOk() {
		t.Errorf("Test with canceled ctx reported ok: %s", resp.GetDiagnostic())
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("Test did not return promptly")
	}
}

func TestBatchBufferingBounded(t *testing.T) {
	s := startServer(t, false)
	big := make([]byte, 1<<20)
	var docs []*connectorv1.DocumentChunk
	for i := 0; i < 70; i++ {
		docs = append(docs, &connectorv1.DocumentChunk{DocumentId: string(rune('A' + i)), Properties: map[string]string{natsconn.PropSubject: "a.b"}, Data: big, Last: true})
	}
	_, err := execDocs(t, s.ClientURL(), createStart(), docs...)
	if err == nil {
		t.Fatal("expected a batch-too-large error, got nil")
	}
}
