// SPDX-License-Identifier: AGPL-3.0-only

package docs

import (
	"errors"
	"io"
	"testing"
)

func TestReleaseWithOpenReaderNoLeak(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(SpoolConfig{Dir: dir, DocThreshold: 4})
	p, err := s.FromBytes([]byte("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := p.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Release(); err != nil {
		t.Fatalf("Release with open reader: %v", err)
	}
	if b, err := io.ReadAll(rc); err != nil || string(b) != "0123456789" {
		t.Fatalf("read after release: %q %v", b, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := spillFiles(t, dir); n != 0 {
		t.Fatalf("leaked %d spill files", n)
	}
}

func TestRetainAfterFreeDoesNotResurrect(t *testing.T) {
	s := NewSpool(SpoolConfig{})
	p, _ := s.FromBytes([]byte("abc"))
	_ = p.Release()
	p.Retain()
	if r := p.refs.Load(); r != 0 {
		t.Fatalf("refs after Retain on closed payload = %d, want 0", r)
	}
	if _, err := p.Open(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Open after Retain-on-closed = %v, want ErrClosed", err)
	}
	if err := p.Release(); err != nil {
		t.Fatal(err)
	}
	if got := s.MemoryInUse(); got != 0 {
		t.Fatalf("memory in use %d", got)
	}
}
