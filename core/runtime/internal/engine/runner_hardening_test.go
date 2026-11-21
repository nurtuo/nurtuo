// SPDX-License-Identifier: AGPL-3.0-only

package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// scriptConn is a connector whose Execute is supplied by the test.
type scriptConn struct {
	fakeConn
	exec func(connectorv1.ConnectorService_ExecuteServer) error
}

func (c scriptConn) Execute(s connectorv1.ConnectorService_ExecuteServer) error { return c.exec(s) }

// inputIDs drains the request stream and returns the ids of the documents sent, in order.
func inputIDs(s connectorv1.ConnectorService_ExecuteServer) []string {
	var order []string
	seen := map[string]bool{}
	for {
		req, err := s.Recv()
		if err != nil {
			return order
		}
		if ch := req.GetChunk(); ch != nil && !seen[ch.GetDocumentId()] {
			seen[ch.GetDocumentId()] = true
			order = append(order, ch.GetDocumentId())
		}
	}
}

func success(id string, outs ...string) *connectorv1.ExecuteResponse {
	return &connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: &connectorv1.DocumentResult{
		DocumentId: id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS, OutputDocumentIds: outs}}}
}

func outChunk(id, data string) *connectorv1.ExecuteResponse {
	return &connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{
		Output: &connectorv1.DocumentChunk{DocumentId: id, Data: []byte(data), Last: true}}}
}

func runScript(t *testing.T, e *engine.Engine, exec func(connectorv1.ConnectorService_ExecuteServer) error, in *docs.Set) *engine.Result {
	t.Helper()
	conns := engine.NewConnectors()
	conns.Register("s", engine.ConnectorEntry{Connector: scriptConn{exec: exec}})
	e.Registry.RegisterConnectorRunner(conns)
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg,
		"c:connector:" + `{"connector":"s","operation":"CREATE"}`,
		"r:returnDocuments:{}",
	}, map[string]string{"start:default": "c", "c:default": "r"}))
	res, err := e.Run(context.Background(), f, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Returned.Close(); _ = res.Failed.Close() })
	return res
}

func allIDs(res *engine.Result) []string {
	got := append(ids(res.Returned), ids(res.Failed)...)
	sort.Strings(got)
	return got
}

func failMsg(t *testing.T, s *docs.Set, i int) string {
	t.Helper()
	if s.Len() <= i || s.At(i).Err() == nil {
		t.Fatalf("no failed doc at %d", i)
	}
	return s.At(i).Err().Message
}

func TestRunnerMissingOutputChunksFailsDocument(t *testing.T) {
	e := engine.New()
	in := docs.NewSet(payloadDoc(t, e, "a", "x", nil))
	res := runScript(t, e, func(s connectorv1.ConnectorService_ExecuteServer) error {
		for _, id := range inputIDs(s) {
			_ = s.Send(success(id, id+".out")) // no output chunks at all
		}
		return nil
	}, in)
	if res.Returned.Len() != 0 || res.Failed.Len() != 1 || res.Failed.At(0).ID != "a" {
		t.Fatalf("returned %v failed %v", ids(res.Returned), ids(res.Failed))
	}
	if m := failMsg(t, res.Failed, 0); !strings.Contains(m, "a.out") {
		t.Fatalf("message %q", m)
	}
}

func TestRunnerSpoolFailureOwnsDocOnce(t *testing.T) {
	e := engine.New()
	in := docs.NewSet(payloadDoc(t, e, "x", "hi", nil))
	e.Spool = docs.NewSpool(docs.SpoolConfig{Dir: filepath.Join(t.TempDir(), "missing", "dir"), DocThreshold: 4})
	res := runScript(t, e, func(s connectorv1.ConnectorService_ExecuteServer) error {
		for _, id := range inputIDs(s) {
			_ = s.Send(outChunk("o1", "0123456789"))
			_ = s.Send(outChunk("o2", "0123456789"))
			_ = s.Send(success(id, "o1", "o2"))
		}
		return nil
	}, in)
	if got := allIDs(res); len(got) != 1 || got[0] != "x" || res.Failed.Len() != 1 {
		t.Fatalf("returned %v failed %v", ids(res.Returned), ids(res.Failed))
	}
}

func TestRunnerDuplicateInputIDsFailDeterministically(t *testing.T) {
	e := engine.New()
	in := docs.NewSet(payloadDoc(t, e, "d", "1", nil), payloadDoc(t, e, "d", "2", nil))
	res := runScript(t, e, func(s connectorv1.ConnectorService_ExecuteServer) error {
		for _, id := range inputIDs(s) {
			_ = s.Send(success(id))
		}
		return nil
	}, in)
	if res.Returned.Len() != 1 || res.Failed.Len() != 1 {
		t.Fatalf("returned %v failed %v", ids(res.Returned), ids(res.Failed))
	}
	if body(t, res.Returned.At(0)) != "1" {
		t.Fatal("first occurrence should pass")
	}
	if m := failMsg(t, res.Failed, 0); !strings.Contains(m, "duplicate") {
		t.Fatalf("message %q", m)
	}
}

func TestRunnerSharedOutputIDFailsBothDocs(t *testing.T) {
	e := engine.New()
	in := docs.NewSet(payloadDoc(t, e, "a", "1", nil), payloadDoc(t, e, "b", "2", nil))
	res := runScript(t, e, func(s connectorv1.ConnectorService_ExecuteServer) error {
		_ = inputIDs(s)
		_ = s.Send(outChunk("same", "{}"))
		_ = s.Send(success("a", "same"))
		_ = s.Send(success("b", "same"))
		return nil
	}, in)
	if res.Returned.Len() != 0 || res.Failed.Len() != 2 {
		t.Fatalf("returned %v failed %v", ids(res.Returned), ids(res.Failed))
	}
	if m := failMsg(t, res.Failed, 0); !strings.Contains(m, "same") {
		t.Fatalf("message %q", m)
	}
}

func TestRunnerStreamErrorAfterSuccessFailsWholeBatch(t *testing.T) {
	e := engine.New()
	in := docs.NewSet(payloadDoc(t, e, "a", "1", nil), payloadDoc(t, e, "b", "2", nil))
	res := runScript(t, e, func(s connectorv1.ConnectorService_ExecuteServer) error {
		_ = inputIDs(s)
		_ = s.Send(success("a"))
		return errors.New("boom")
	}, in)
	if res.Returned.Len() != 0 || res.Failed.Len() != 2 {
		t.Fatalf("returned %v failed %v", ids(res.Returned), ids(res.Failed))
	}
	for i := 0; i < 2; i++ {
		if m := failMsg(t, res.Failed, i); !strings.Contains(m, "boom") {
			t.Fatalf("message %q", m)
		}
	}
}

func TestRunnerRecoversConnectorPanic(t *testing.T) {
	e := engine.New()
	in := docs.NewSet(payloadDoc(t, e, "a", "1", nil), payloadDoc(t, e, "b", "2", nil))
	res := runScript(t, e, func(s connectorv1.ConnectorService_ExecuteServer) error {
		_ = inputIDs(s)
		panic("kaboom")
	}, in)
	if res.Returned.Len() != 0 || res.Failed.Len() != 2 {
		t.Fatalf("returned %v failed %v", ids(res.Returned), ids(res.Failed))
	}
	if m := failMsg(t, res.Failed, 0); !strings.Contains(m, "kaboom") {
		t.Fatalf("message %q", m)
	}
}
