// SPDX-License-Identifier: AGPL-3.0-only

package docs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned when a payload is used after its last reference was released.
var ErrClosed = errors.New("docs: payload closed")

// Default limits for a Spool.
const (
	DefaultDocThreshold = 1 << 20  // 1 MiB per document held in memory
	DefaultTotalBudget  = 64 << 20 // 64 MiB across all in-memory payloads of a Spool
)

// Spool creates payloads, keeping small ones in memory and spilling large ones to
// temp files. Memory is bounded two ways: a per-document threshold and a total
// budget shared by every payload the spool created. Safe for concurrent use.
type Spool struct {
	dir          string
	docThreshold int64
	totalBudget  int64
	used         atomic.Int64
	spilled      atomic.Int64 // number of payloads ever spilled
}

// SpoolConfig configures a Spool. Zero values select defaults; Dir "" uses os.TempDir.
type SpoolConfig struct {
	Dir          string
	DocThreshold int64
	TotalBudget  int64
}

// NewSpool returns a Spool for the given config.
func NewSpool(cfg SpoolConfig) *Spool {
	if cfg.DocThreshold <= 0 {
		cfg.DocThreshold = DefaultDocThreshold
	}
	if cfg.TotalBudget <= 0 {
		cfg.TotalBudget = DefaultTotalBudget
	}
	return &Spool{dir: cfg.Dir, docThreshold: cfg.DocThreshold, totalBudget: cfg.TotalBudget}
}

// MemoryInUse reports bytes currently held in memory by live payloads.
func (s *Spool) MemoryInUse() int64 { return s.used.Load() }

// SpilledCount reports how many payloads have been spilled to disk so far.
func (s *Spool) SpilledCount() int64 { return s.spilled.Load() }

// reserve tries to take n bytes of the shared budget.
func (s *Spool) reserve(n int64) bool {
	for {
		cur := s.used.Load()
		if cur+n > s.totalBudget {
			return false
		}
		if s.used.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// NewPayload consumes r and returns a payload. At most docThreshold+1 bytes are
// buffered before the decision to spill is made; spilled data is streamed to disk.
func (s *Spool) NewPayload(r io.Reader) (*Payload, error) {
	if r == nil {
		return s.fromBytes(nil), nil
	}
	var bb bytes.Buffer
	n, err := io.Copy(&bb, io.LimitReader(r, s.docThreshold+1))
	if err != nil {
		return nil, err
	}
	if n <= s.docThreshold {
		return s.fromBytes(bb.Bytes()), nil
	}
	return s.spillFrom(io.MultiReader(&bb, r))
}

// FromBytes wraps b (not copied) as a payload, spilling if it is too big.
func (s *Spool) FromBytes(b []byte) (*Payload, error) {
	if int64(len(b)) <= s.docThreshold {
		return s.fromBytes(b), nil
	}
	return s.spillFrom(bytes.NewReader(b))
}

func (s *Spool) fromBytes(b []byte) *Payload {
	n := int64(len(b))
	held := n
	if n > 0 && !s.reserve(n) {
		if p, err := s.spillFrom(bytes.NewReader(b)); err == nil {
			return p
		}
		// Disk failed: keep in memory over budget rather than losing data.
		s.used.Add(n)
	}
	p := &Payload{spool: s, mem: b, size: n, held: held}
	p.refs.Store(1)
	return p
}

func (s *Spool) spillFrom(r io.Reader) (*Payload, error) {
	f, err := os.CreateTemp(s.dir, "nurtuo-spill-*")
	if err != nil {
		return nil, fmt.Errorf("docs: create spill file: %w", err)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("docs: write spill file: %w", err)
	}
	s.spilled.Add(1)
	p := &Payload{spool: s, path: f.Name(), size: n}
	p.refs.Store(1)
	return p, nil
}

// Payload is an immutable, re-readable byte stream, in memory or on disk. It is
// reference counted: Retain adds an owner, Release drops one; the last Release
// frees memory budget and deletes any spill file.
type Payload struct {
	spool *Spool
	mem   []byte
	path  string
	size  int64
	held  int64 // bytes reserved from spool budget
	refs  atomic.Int32
	mu    sync.Mutex
	done  bool
	open  int // outstanding spill readers; guarded by mu
}

// Size returns the payload length in bytes.
func (p *Payload) Size() int64 { return p.size }

// Spilled reports whether the payload lives on disk.
func (p *Payload) Spilled() bool { return p.path != "" }

// Open returns a fresh reader positioned at the start. Each call is independent.
func (p *Payload) Open() (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return nil, ErrClosed
	}
	if p.path != "" {
		f, err := os.Open(p.path)
		if err != nil {
			return nil, err
		}
		p.open++
		return &spillReader{File: f, p: p}, nil
	}
	return io.NopCloser(bytes.NewReader(p.mem)), nil
}

// spillReader tracks open handles so the spill file can be removed on the last
// Close when the payload was freed while readers were still open (required on
// Windows, harmless elsewhere).
type spillReader struct {
	*os.File
	p    *Payload
	once sync.Once
}

func (r *spillReader) Close() error {
	var rerr error
	r.once.Do(func() {
		err := r.File.Close()
		r.p.mu.Lock()
		defer r.p.mu.Unlock()
		r.p.open--
		if r.p.done && r.p.open == 0 {
			if e := os.Remove(r.p.path); e != nil && !os.IsNotExist(e) && err == nil {
				err = e
			}
		}
		rerr = err
	})
	return rerr
}

// Bytes reads the whole payload into memory. Intended for small payloads and tests.
func (p *Payload) Bytes() ([]byte, error) {
	rc, err := p.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// Retain adds an owner and returns p. Retaining a payload whose last reference
// was already released is a documented no-op: a closed payload is never
// resurrected, and Open on it keeps returning ErrClosed.
func (p *Payload) Retain() *Payload {
	for {
		cur := p.refs.Load()
		if cur <= 0 {
			return p
		}
		if p.refs.CompareAndSwap(cur, cur+1) {
			return p
		}
	}
}

// Release drops one owner; the last one frees resources. Extra releases are no-ops.
func (p *Payload) Release() error {
	if p == nil {
		return nil
	}
	for {
		cur := p.refs.Load()
		if cur <= 0 {
			return nil
		}
		if p.refs.CompareAndSwap(cur, cur-1) {
			if cur == 1 {
				return p.free()
			}
			return nil
		}
	}
}

func (p *Payload) free() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return nil
	}
	p.done = true
	p.spool.used.Add(-p.held)
	p.mem = nil
	if p.path != "" && p.open == 0 {
		if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
