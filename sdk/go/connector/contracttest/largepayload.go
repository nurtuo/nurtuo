// SPDX-License-Identifier: Apache-2.0

package contracttest

import (
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/proto"
)

// Defaults for LargePayloadCase. The payload is deliberately larger than the 4 MiB gRPC default
// message size, so it can only be delivered as several chunks, and large enough that a connector
// which buffers the whole document exceeds the heap bound.
const (
	DefaultLargePayloadSize = 64 << 20
	DefaultMaxHeapGrowth    = 24 << 20
)

// LargePayloadCase configures the ExecuteLargePayload subtest: one document of Size bytes is
// streamed to the connector in ChunkSize pieces without ever being held in memory by the suite,
// and the live heap of the process must not grow by more than MaxHeapGrowth while it is handled.
//
// Only connectors that process a document incrementally (for example an SFTP upload) can provide
// a case; a connector that by design buffers one document (HTTP bodies, NATS messages) leaves
// Options.LargePayload nil and the subtest is skipped for it. The heap bound is measured in the
// test process, so it only applies to in-process connectors (Options.New).
type LargePayloadCase struct {
	// Start is the first message of the stream, as in ExecuteCase.
	Start *connectorv1.ExecuteStart
	// Properties are attached to the first chunk (for example the destination path).
	Properties map[string]string
	// Size is the payload size in bytes; defaults to DefaultLargePayloadSize and must be > 4 MiB.
	Size int
	// ChunkSize defaults to DefaultChunkSize.
	ChunkSize int
	// MaxHeapGrowth is the allowed growth of live heap bytes; defaults to DefaultMaxHeapGrowth.
	MaxHeapGrowth uint64
	// Want is the required status of the document; defaults to SUCCESS.
	Want connectorv1.DocumentStatus
}

const largeDocID = "contracttest-large"

func (c LargePayloadCase) withDefaults() LargePayloadCase {
	if c.Size == 0 {
		c.Size = DefaultLargePayloadSize
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = DefaultChunkSize
	}
	if c.MaxHeapGrowth == 0 {
		c.MaxHeapGrowth = DefaultMaxHeapGrowth
	}
	if c.Want == connectorv1.DocumentStatus_DOCUMENT_STATUS_UNSPECIFIED {
		c.Want = connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
	}
	return c
}

func executeLargePayload(t *testing.T, o *Options) {
	if o.LargePayload == nil {
		t.Skip("Options.LargePayload not set: this connector buffers a whole document by design")
	}
	if o.Dial != nil {
		t.Skip("heap growth is measured in the test process; not applicable to a connector behind Options.Dial")
	}
	c := o.LargePayload.withDefaults()
	if c.Size <= 4<<20 {
		t.Fatalf("LargePayloadCase.Size = %d, must exceed 4 MiB", c.Size)
	}
	res, growth, err := streamLargePayload(t, o, c)
	if err != nil {
		t.Fatal(err)
	}
	if res.GetStatus() != c.Want {
		t.Errorf("large document: status %v (%s), want %v", res.GetStatus(), res.GetMessage(), c.Want)
	}
	if growth > c.MaxHeapGrowth {
		t.Errorf("live heap grew by %d bytes while streaming %d bytes, limit %d: the connector buffers the document instead of streaming it",
			growth, c.Size, c.MaxHeapGrowth)
	}
}

// streamLargePayload streams the payload and returns the document result and the peak growth of
// the live heap (bytes reachable after a GC) over the baseline taken before the stream starts.
func streamLargePayload(t *testing.T, o *Options, c LargePayloadCase) (*connectorv1.DocumentResult, uint64, error) {
	cl := client(t, o)
	stream, err := cl.Execute(ctx(t, o))
	if err != nil {
		return nil, 0, err
	}
	start := &connectorv1.ExecuteStart{}
	if c.Start != nil {
		start = proto.Clone(c.Start).(*connectorv1.ExecuteStart)
	}
	if start.ExecutionId == "" {
		start.ExecutionId = "contracttest-exec"
	}
	if start.StepId == "" {
		start.StepId = "contracttest-step"
	}
	if start.Connection == nil {
		start.Connection = o.Connection
	}

	base := liveHeap()
	var (
		mu   sync.Mutex
		peak = base
	)
	sample := func() {
		h := liveHeap()
		mu.Lock()
		if h > peak {
			peak = h
		}
		mu.Unlock()
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				sample()
			}
		}
	}()
	// A failed Send only reports io.EOF; the real status is returned by Recv.
	sendErr := func(err error) error {
		if errors.Is(err, io.EOF) {
			for {
				if _, rerr := stream.Recv(); rerr != nil && !errors.Is(rerr, io.EOF) {
					return rerr
				} else if rerr != nil {
					break
				}
			}
		}
		return err
	}
	finish := func() uint64 {
		close(stop)
		<-done
		sample()
		mu.Lock()
		defer mu.Unlock()
		if peak < base {
			return 0
		}
		return peak - base
	}

	if err := stream.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Start{Start: start}}); err != nil {
		finish()
		return nil, 0, sendErr(err)
	}
	// One reusable buffer; the marshaller copies it, so the suite itself stays at ChunkSize bytes.
	buf := make([]byte, c.ChunkSize)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	for off := 0; off < c.Size; off += c.ChunkSize {
		n := min(c.ChunkSize, c.Size-off)
		ch := &connectorv1.DocumentChunk{DocumentId: largeDocID, Data: buf[:n], Last: off+n == c.Size}
		if off == 0 {
			ch.Properties = c.Properties
		}
		if err := stream.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: ch}}); err != nil {
			finish()
			return nil, 0, sendErr(err)
		}
	}
	if err := stream.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}}); err != nil {
		finish()
		return nil, 0, err
	}
	if err := stream.CloseSend(); err != nil {
		finish()
		return nil, 0, err
	}

	var result *connectorv1.DocumentResult
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			finish()
			return nil, 0, err
		}
		if r := resp.GetResult(); r != nil && r.GetDocumentId() == largeDocID {
			result = r
		}
	}
	growth := finish()
	if result == nil {
		return nil, growth, errors.New("no DocumentResult for the large document")
	}
	return result, growth, nil
}

// liveHeap returns the bytes of heap objects still reachable after a full GC.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
