// SPDX-License-Identifier: AGPL-3.0-only

// Package docs holds the flow-independent document model of the engine: documents
// with streamed payloads, sets with disk spill, per-document status, and a
// set-at-a-time pipeline primitive.
package docs

import (
	"io"
	"maps"
)

// Document is {id, props, payload, status}. A Document is owned by one goroutine
// at a time; the pipeline guarantees that for the functions it calls.
type Document struct {
	ID      string
	Props   map[string]string
	payload *Payload
	status  Status
	err     *Error
}

// New creates an OK document owning payload (which may be nil for an empty body).
func New(id string, props map[string]string, payload *Payload) *Document {
	if props == nil {
		props = map[string]string{}
	}
	return &Document{ID: id, Props: props, payload: payload}
}

// Payload returns the payload, or nil if empty/released.
func (d *Document) Payload() *Payload { return d.payload }

// Open returns a reader over the payload (empty reader if none).
func (d *Document) Open() (io.ReadCloser, error) {
	if d.payload == nil {
		return io.NopCloser(eofReader{}), nil
	}
	return d.payload.Open()
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// SetPayload replaces the payload, releasing the previous one.
func (d *Document) SetPayload(p *Payload) {
	old := d.payload
	d.payload = p
	if old != p {
		_ = old.Release()
	}
}

// Status returns the document status.
func (d *Document) Status() Status { return d.status }

// Err returns the error detail for StatusError documents, else nil.
func (d *Document) Err() *Error { return d.err }

// Fail marks the document as errored.
func (d *Document) Fail(e *Error) {
	if e == nil {
		e = &Error{Message: "unknown error"}
	}
	d.status, d.err = StatusError, e
}

// Drop marks the document as intentionally dropped (filtered out).
func (d *Document) Drop() { d.status, d.err = StatusDropped, nil }

// Clone copies props and shares the payload (reference counted). Status is reset to OK.
func (d *Document) Clone(newID string) *Document {
	c := &Document{ID: newID, Props: maps.Clone(d.Props)}
	if d.payload != nil {
		c.payload = d.payload.Retain()
	}
	return c
}

// Release drops the document's payload reference.
func (d *Document) Release() error {
	p := d.payload
	d.payload = nil
	return p.Release()
}

// ClearError resets an errored document to OK and returns the error it carried (nil if none).
// Used by Try/Catch to route failed documents down the catch path.
func (d *Document) ClearError() *Error {
	e := d.err
	d.status, d.err = StatusOK, nil
	return e
}
