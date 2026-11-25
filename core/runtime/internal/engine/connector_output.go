// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"fmt"
	"io"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// The native SFTP connector can use the remaining budget to stop reading
// before it produces an oversized chunk or directory listing.
func (s *memStream) OutputBytesRemaining() (int64, bool) {
	if s.bounded == nil {
		return 0, false
	}
	return s.bounded.remaining, true
}

func (s *memStream) RejectOutputLimit() error {
	s.bounded.err = ErrReturnLimit
	return ErrReturnLimit
}

type boundedOutputs struct {
	remaining int64
	spool     *docs.Spool
	docs      map[string]*outDoc
	err       error
}

type outputPipe struct {
	writer *io.PipeWriter
	done   chan payloadResult
}

type payloadResult struct {
	payload *docs.Payload
	err     error
}

func startOutputPipe(spool *docs.Spool) *outputPipe {
	reader, writer := io.Pipe()
	p := &outputPipe{writer: writer, done: make(chan payloadResult, 1)}
	go func() {
		payload, err := spool.NewPayload(reader)
		_ = reader.CloseWithError(err)
		p.done <- payloadResult{payload, err}
	}()
	return p
}

func (b *boundedOutputs) send(ch *connectorv1.DocumentChunk) error {
	size := int64(len(ch.GetData()))
	if size > b.remaining {
		b.err = ErrReturnLimit
		return b.err
	}
	b.remaining -= size
	id := ch.GetDocumentId()
	o := b.docs[id]
	if o == nil {
		o = &outDoc{props: map[string]string{}, pending: startOutputPipe(b.spool)}
		b.docs[id] = o
	}
	if o.pending == nil {
		b.err = fmt.Errorf("connector sent an output chunk after the final chunk for document %q", id)
		return b.err
	}
	for k, v := range ch.GetProperties() {
		o.props[k] = v
	}
	if size > 0 {
		if _, err := o.pending.writer.Write(ch.GetData()); err != nil {
			b.err = err
			return err
		}
	}
	if ch.GetLast() {
		_ = o.pending.writer.Close()
		result := <-o.pending.done
		o.pending = nil
		o.payload, b.err = result.payload, result.err
	}
	return b.err
}

func (o *outDoc) takePayload(spool *docs.Spool) (*docs.Payload, error) {
	if o.pending != nil {
		return nil, fmt.Errorf("connector output is missing its final chunk")
	}
	if o.payload != nil {
		payload := o.payload
		o.payload = nil
		return payload, nil
	}
	return spool.FromBytes(o.data.Bytes())
}

func (b *boundedOutputs) close() {
	for _, o := range b.docs {
		if o.pending != nil {
			_ = o.pending.writer.CloseWithError(io.ErrUnexpectedEOF)
			o.payload = (<-o.pending.done).payload
		}
		if o.payload != nil {
			_ = o.payload.Release()
		}
	}
}
