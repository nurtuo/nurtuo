// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/maps"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// ConnectorEntry is a connector the runtime can invoke in-process.
type ConnectorEntry struct {
	Connector connector.Connector
	// Connection is the resolved connection config (secrets injected by the runtime).
	Connection *connectorv1.ConnectionConfig
	// Options are passed as ExecuteStart.options (connector-specific, e.g. an HTTP path).
	Options *structpb.Struct
}

// Connectors is a registry of in-process connectors keyed by the flow's connector id. A step's
// optional `connection` name selects the entry registered as "<connector>/<connection>".
type Connectors struct {
	mu sync.RWMutex
	m  map[string]ConnectorEntry
}

// NewConnectors returns an empty registry.
func NewConnectors() *Connectors { return &Connectors{m: map[string]ConnectorEntry{}} }

// Register adds or replaces an entry.
func (c *Connectors) Register(key string, e ConnectorEntry) {
	c.mu.Lock()
	c.m[key] = e
	c.mu.Unlock()
}

// ConnectorLookup resolves the connector instance a connector step runs against. flowID is the
// executing flow, so a runtime can scope connections per deployment.
type ConnectorLookup interface {
	Lookup(flowID string, cfg flow.ConnectorConfig) (ConnectorEntry, error)
}

// Lookup implements ConnectorLookup (the flow id is ignored: the registry is flat).
func (c *Connectors) Lookup(_ string, cfg flow.ConnectorConfig) (ConnectorEntry, error) {
	return c.lookup(cfg)
}

func (c *Connectors) lookup(cfg flow.ConnectorConfig) (ConnectorEntry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if cfg.Connection != "" {
		if e, ok := c.m[cfg.Connector+"/"+cfg.Connection]; ok {
			return e, nil
		}
	}
	if e, ok := c.m[cfg.Connector]; ok {
		return e, nil
	}
	return ConnectorEntry{}, fmt.Errorf("connector %q (connection %q) is not registered", cfg.Connector, cfg.Connection)
}

// RegisterConnectorRunner installs the `connector` step runner backed by conns.
func (r *Registry) RegisterConnectorRunner(conns *Connectors) {
	r.Register(flow.KindConnector, ConnectorRunner(conns))
}

// RegisterConnectorLookup installs the `connector` step runner backed by an arbitrary lookup
// (for example the runtime's per-deployment connection manager).
func (r *Registry) RegisterConnectorLookup(l ConnectorLookup) {
	r.Register(flow.KindConnector, ConnectorRunnerFor(l))
}

// RegisterMapRunner installs the `map` step runner (JSONata via the maps package).
func (r *Registry) RegisterMapRunner() { r.Register(flow.KindMap, MapRunner()) }

// chunkSize is the payload chunk size sent to connectors.
const chunkSize = 64 * 1024

// ConnectorRunner returns a StepRunner that sends the whole OK document set to the connector's
// Execute stream (one stream per step execution) and maps DocumentResults back onto documents:
//
//   - SUCCESS with output documents: the input is replaced by its outputs (input props are
//     inherited, output props win); SUCCESS without outputs: the input passes through unchanged.
//   - APPLICATION_ERROR / FAILURE (or no result at all): the document is failed with the message.
//   - Tracked values of a result are exposed as document properties "tracked.<name>".
//
// A stream-level error fails every document of the set. Non-OK input documents pass through.
func ConnectorRunner(conns *Connectors) StepRunner { return ConnectorRunnerFor(conns) }

// ConnectorRunnerFor is ConnectorRunner over any ConnectorLookup.
func ConnectorRunnerFor(conns ConnectorLookup) StepRunner {
	return StepRunnerFunc(func(ctx context.Context, sc *StepContext, in *docs.Set) (*docs.Set, error) {
		cfg := sc.Config.(flow.ConnectorConfig)
		entry, err := conns.Lookup(sc.Flow.ID, cfg)
		if err != nil {
			return in, err
		}
		start := &connectorv1.ExecuteStart{
			ExecutionId:  fmt.Sprintf("%s/%s", sc.Flow.ID, sc.Step.ID),
			StepId:       sc.Step.ID,
			Connection:   entry.Connection,
			ObjectTypeId: cfg.ObjectType,
			Options:      entry.Options,
		}
		start.OperationType, start.CustomType = operationType(cfg.Operation)

		stream := &memStream{ctx: ctx}
		stream.reqs = append(stream.reqs, &connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Start{Start: start}})
		// Documents are matched to results by id, so an id may be sent only once per batch: later
		// occurrences are failed instead of sharing the first one's result.
		var sent []*docs.Document
		sentIDs := map[string]bool{}
		for _, d := range in.Docs() {
			if d.Status() != docs.StatusOK {
				continue
			}
			if sentIDs[d.ID] {
				d.Fail(&docs.Error{Step: sc.Step.ID, Code: "duplicate_document_id",
					Message: fmt.Sprintf("duplicate document id %q in batch", d.ID)})
				continue
			}
			if err := appendChunks(stream, d); err != nil {
				d.Fail(&docs.Error{Step: sc.Step.ID, Code: "payload", Message: err.Error(), Cause: err})
				continue
			}
			sentIDs[d.ID] = true
			sent = append(sent, d)
		}
		stream.reqs = append(stream.reqs, &connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}})

		execErr := safeExecute(entry.Connector, stream)
		if execErr == nil {
			execErr = ctx.Err()
		}
		results := map[string][]*connectorv1.DocumentResult{}
		for _, r := range stream.resps {
			if res := r.GetResult(); res != nil {
				results[res.GetDocumentId()] = append(results[res.GetDocumentId()], res)
			}
		}
		outputs := collectOutputs(stream.resps)
		// An output id may be claimed by exactly one result entry.
		outRefs := map[string]int{}
		for id := range sentIDs {
			if rs := results[id]; len(rs) == 1 {
				for _, oid := range rs[0].GetOutputDocumentIds() {
					outRefs[oid]++
				}
			}
		}

		fail := func(d *docs.Document, code, msg string, cause error) {
			d.Fail(&docs.Error{Step: sc.Step.ID, Code: code, Message: msg, Cause: cause})
		}
		isSent := make(map[*docs.Document]bool, len(sent))
		for _, d := range sent {
			isSent[d] = true
		}

		out := docs.NewSet()
		for _, d := range in.Docs() {
			if !isSent[d] {
				out.Add(d) // non-OK, duplicate or payload-failed above
				continue
			}
			rs := results[d.ID]
			switch {
			case execErr != nil:
				// A non-OK stream status fails the whole batch, even for documents that got a result.
				fail(d, "connector", execErr.Error(), execErr)
				out.Add(d)
			case len(rs) == 0:
				fail(d, "no_result", "connector returned no result for document", nil)
				out.Add(d)
			case len(rs) > 1:
				fail(d, "duplicate_result", fmt.Sprintf("connector returned %d results for document %q", len(rs), d.ID), nil)
				out.Add(d)
			case rs[0].GetStatus() != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS:
				res := rs[0]
				code := "application_error"
				if res.GetStatus() == connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE {
					code = "failure"
				} else if res.GetStatus() == connectorv1.DocumentStatus_DOCUMENT_STATUS_UNSPECIFIED {
					code = "unspecified_status"
				}
				msg := res.GetMessage()
				if msg == "" {
					msg = res.GetStatus().String()
				}
				fail(d, code, msg, nil)
				out.Add(d)
			default:
				res := rs[0]
				for k, v := range res.GetTracked() {
					d.Props["tracked."+k] = v
				}
				outIDs := res.GetOutputDocumentIds()
				if len(outIDs) == 0 {
					out.Add(d)
					continue
				}
				// Build every output first; the input is replaced only if all of them succeed, so
				// it ends up in the output set exactly once either way.
				built := make([]*docs.Document, 0, len(outIDs))
				var ferr error
				var fcode string
				for _, id := range outIDs {
					o, ok := outputs[id]
					switch {
					case outRefs[id] > 1:
						ferr, fcode = fmt.Errorf("output document id %q is referenced more than once", id), "duplicate_output_id"
					case !ok:
						ferr, fcode = fmt.Errorf("output document %q listed in result has no output chunks", id), "missing_output"
					}
					if ferr != nil {
						break
					}
					p, err := sc.Spool.FromBytes(o.data.Bytes())
					if err != nil {
						ferr, fcode = err, "payload"
						break
					}
					built = append(built, docs.New(id, mergeProps(d.Props, o.props), p))
				}
				if ferr != nil {
					for _, nd := range built {
						_ = nd.Release()
					}
					fail(d, fcode, ferr.Error(), ferr)
					out.Add(d)
					continue
				}
				for _, nd := range built {
					out.Add(nd)
				}
				_ = d.Release()
			}
		}
		return out, nil
	})
}

// safeExecute runs connector.Execute, converting a panic into an error.
func safeExecute(c connector.Connector, s connectorv1.ConnectorService_ExecuteServer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("connector panicked: %v", r)
		}
	}()
	return c.Execute(s)
}

func mergeProps(base, over map[string]string) map[string]string {
	m := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		m[k] = v
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func operationType(op string) (connectorv1.OperationType, string) {
	switch strings.ToUpper(op) {
	case "GET":
		return connectorv1.OperationType_OPERATION_TYPE_GET, ""
	case "QUERY":
		return connectorv1.OperationType_OPERATION_TYPE_QUERY, ""
	case "CREATE":
		return connectorv1.OperationType_OPERATION_TYPE_CREATE, ""
	case "UPDATE":
		return connectorv1.OperationType_OPERATION_TYPE_UPDATE, ""
	case "UPSERT":
		return connectorv1.OperationType_OPERATION_TYPE_UPSERT, ""
	case "DELETE":
		return connectorv1.OperationType_OPERATION_TYPE_DELETE, ""
	}
	return connectorv1.OperationType_OPERATION_TYPE_EXECUTE, op
}

func appendChunks(s *memStream, d *docs.Document) error {
	r, err := d.Open()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	buf := make([]byte, chunkSize)
	first := true
	for {
		n, err := io.ReadFull(r, buf)
		last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !last {
			return err
		}
		ch := &connectorv1.DocumentChunk{DocumentId: d.ID, Data: bytes.Clone(buf[:n]), Last: last}
		if first {
			ch.Properties = mergeProps(d.Props, nil)
			first = false
		}
		if n > 0 || ch.Last {
			s.reqs = append(s.reqs, &connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: ch}})
		}
		if last {
			return nil
		}
	}
}

type outDoc struct {
	props map[string]string
	data  bytes.Buffer
}

func collectOutputs(resps []*connectorv1.ExecuteResponse) map[string]*outDoc {
	m := map[string]*outDoc{}
	for _, r := range resps {
		ch := r.GetOutput()
		if ch == nil {
			continue
		}
		o := m[ch.GetDocumentId()]
		if o == nil {
			o = &outDoc{props: map[string]string{}}
			m[ch.GetDocumentId()] = o
		}
		for k, v := range ch.GetProperties() {
			o.props[k] = v
		}
		o.data.Write(ch.GetData())
	}
	return m
}

// memStream is an in-memory connector Execute stream: all requests are pre-buffered and responses
// are collected.
type memStream struct {
	grpc.ServerStream
	ctx   context.Context
	reqs  []*connectorv1.ExecuteRequest
	i     int
	resps []*connectorv1.ExecuteResponse
}

func (s *memStream) Context() context.Context { return s.ctx }

func (s *memStream) Recv() (*connectorv1.ExecuteRequest, error) {
	if s.i >= len(s.reqs) {
		return nil, io.EOF
	}
	r := s.reqs[s.i]
	s.i++
	return r, nil
}

func (s *memStream) Send(r *connectorv1.ExecuteResponse) error {
	s.resps = append(s.resps, r)
	return nil
}

// MapRunner returns a StepRunner that parses each document payload as JSON, evaluates the step's
// JSONata expression (with the maps helper functions) and replaces the payload with the JSON result.
// An undefined result drops the document.
func MapRunner() StepRunner {
	var mu sync.Mutex
	cache := map[string]*maps.Transform{}
	compile := func(expr string) (*maps.Transform, error) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := cache[expr]; ok {
			return t, nil
		}
		t, err := maps.CompileTransform(expr)
		if err != nil {
			return nil, err
		}
		cache[expr] = t
		return t, nil
	}
	return StepRunnerFunc(func(ctx context.Context, sc *StepContext, in *docs.Set) (*docs.Set, error) {
		cfg := sc.Config.(flow.MapConfig)
		t, err := compile(cfg.JSONata)
		if err != nil {
			return in, &docs.Error{Step: sc.Step.ID, Code: "map_compile", Message: err.Error(), Cause: err}
		}
		return docs.Apply(ctx, in, docs.Options{Step: sc.Step.ID}, func(_ context.Context, d *docs.Document) (*docs.Document, error) {
			var src any
			if d.Payload() != nil {
				b, err := d.Payload().Bytes()
				if err != nil {
					return nil, err
				}
				if len(bytes.TrimSpace(b)) > 0 {
					if err := json.Unmarshal(b, &src); err != nil {
						return nil, fmt.Errorf("map: payload is not JSON: %w", err)
					}
				}
			}
			v, ok, err := t.Eval(src)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, nil
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			p, err := sc.Spool.FromBytes(b)
			if err != nil {
				return nil, err
			}
			d.SetPayload(p)
			return d, nil
		})
	})
}
