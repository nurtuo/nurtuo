// SPDX-License-Identifier: AGPL-3.0-only

package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func spillFiles(t testing.TB, dir string) int {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

func TestSpoolPayload(t *testing.T) {
	tests := []struct {
		name        string
		cfg         SpoolConfig
		size        int
		wantSpilled bool
	}{
		{"empty", SpoolConfig{DocThreshold: 10}, 0, false},
		{"below threshold", SpoolConfig{DocThreshold: 10}, 9, false},
		{"at threshold", SpoolConfig{DocThreshold: 10}, 10, false},
		{"above threshold", SpoolConfig{DocThreshold: 10}, 11, true},
		{"large 8MiB", SpoolConfig{DocThreshold: 1 << 20}, 8 << 20, true},
		{"total budget exceeded", SpoolConfig{DocThreshold: 100, TotalBudget: 5}, 50, true},
	}
	for _, tc := range tests {
		for _, mode := range []string{"reader", "bytes"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				tc.cfg.Dir = dir
				sp := NewSpool(tc.cfg)
				data := bytes.Repeat([]byte("x"), tc.size)
				var p *Payload
				var err error
				if mode == "reader" {
					p, err = sp.NewPayload(bytes.NewReader(data))
				} else {
					p, err = sp.FromBytes(data)
				}
				if err != nil {
					t.Fatal(err)
				}
				if p.Spilled() != tc.wantSpilled {
					t.Fatalf("spilled=%v want %v", p.Spilled(), tc.wantSpilled)
				}
				if p.Size() != int64(tc.size) {
					t.Fatalf("size=%d", p.Size())
				}
				for i := 0; i < 2; i++ { // re-readable
					got, err := p.Bytes()
					if err != nil || !bytes.Equal(got, data) {
						t.Fatalf("read %d: err=%v len=%d", i, err, len(got))
					}
				}
				wantFiles := 0
				if tc.wantSpilled {
					wantFiles = 1
				}
				if n := spillFiles(t, dir); n != wantFiles {
					t.Fatalf("files=%d want %d", n, wantFiles)
				}
				if err := p.Release(); err != nil {
					t.Fatal(err)
				}
				if n := spillFiles(t, dir); n != 0 {
					t.Fatalf("files after release=%d", n)
				}
				if sp.MemoryInUse() != 0 {
					t.Fatalf("memory leak: %d", sp.MemoryInUse())
				}
				if _, err := p.Open(); !errors.Is(err, ErrClosed) {
					t.Fatalf("open after release: %v", err)
				}
			})
		}
	}
}

func TestPayloadRefCount(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(SpoolConfig{Dir: dir, DocThreshold: 4})
	p, _ := sp.FromBytes([]byte("0123456789"))
	p.Retain()
	_ = p.Release()
	if spillFiles(t, dir) != 1 {
		t.Fatal("file removed too early")
	}
	if _, err := p.Bytes(); err != nil {
		t.Fatal(err)
	}
	_ = p.Release()
	_ = p.Release() // extra release is a no-op
	if spillFiles(t, dir) != 0 {
		t.Fatal("file not removed")
	}
	var nilP *Payload
	if nilP.Release() != nil {
		t.Fatal("nil release")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestNewPayloadReadError(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(SpoolConfig{Dir: dir, DocThreshold: 4})
	r := io.MultiReader(strings.NewReader("0123456789"), errReader{})
	if _, err := sp.NewPayload(r); err == nil {
		t.Fatal("expected error")
	}
	if spillFiles(t, dir) != 0 {
		t.Fatal("spill file leaked on error")
	}
	if sp.MemoryInUse() != 0 {
		t.Fatal("budget leaked")
	}
}

func TestMemoryBounded(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(SpoolConfig{Dir: dir, DocThreshold: 100, TotalBudget: 250})
	set := NewSet()
	for i := 0; i < 20; i++ {
		p, err := sp.FromBytes(bytes.Repeat([]byte("a"), 80))
		if err != nil {
			t.Fatal(err)
		}
		set.Add(New(fmt.Sprint(i), nil, p))
		if sp.MemoryInUse() > 250 {
			t.Fatalf("budget exceeded: %d", sp.MemoryInUse())
		}
	}
	if sp.SpilledCount() != 17 { // 3 fit (240), the rest spill
		t.Fatalf("spilled=%d", sp.SpilledCount())
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if err := set.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if spillFiles(t, dir) != 0 || sp.MemoryInUse() != 0 {
		t.Fatal("cleanup incomplete")
	}
}

func TestLargePayloadStreamsAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(SpoolConfig{Dir: dir, DocThreshold: 64 << 10})
	const size = 64 << 20
	p, err := sp.NewPayload(io.LimitReader(zeroReader{}, size))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Spilled() || p.Size() != size {
		t.Fatalf("spilled=%v size=%d", p.Spilled(), p.Size())
	}
	if sp.MemoryInUse() != 0 {
		t.Fatal("spilled payload holds memory")
	}
	rc, _ := p.Open()
	n, _ := io.Copy(io.Discard, rc)
	_ = rc.Close()
	if n != size {
		t.Fatalf("read %d", n)
	}
	_ = p.Release()
	if spillFiles(t, dir) != 0 {
		t.Fatal("not cleaned")
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestDocumentStatusAndClone(t *testing.T) {
	sp := NewSpool(SpoolConfig{Dir: t.TempDir(), DocThreshold: 4})
	p, _ := sp.FromBytes([]byte("hello world"))
	d := New("a", map[string]string{"k": "v"}, p)
	if d.Status() != StatusOK || d.Err() != nil {
		t.Fatal("new doc not ok")
	}
	c := d.Clone("b")
	c.Props["k"] = "changed"
	if d.Props["k"] != "v" {
		t.Fatal("props shared")
	}
	d.Fail(&Error{Code: "E1", Message: "bad", Step: "map"})
	if d.Status() != StatusError || d.Err().Error() != "map: E1: bad" {
		t.Fatalf("%v %v", d.Status(), d.Err())
	}
	if c.Status() != StatusOK {
		t.Fatal("clone status")
	}
	d.Drop()
	if d.Status() != StatusDropped || d.Err() != nil {
		t.Fatal("drop")
	}
	_ = d.Release()
	if b, err := c.Payload().Bytes(); err != nil || string(b) != "hello world" {
		t.Fatalf("clone payload lost: %v", err)
	}
	// empty doc reads empty
	rc, err := New("e", nil, nil).Open()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Fatal("not empty")
	}
	for s, want := range map[Status]string{StatusOK: "ok", StatusError: "error", StatusDropped: "dropped", 9: "status(9)"} {
		if s.String() != want {
			t.Fatalf("%v", s)
		}
	}
	cause := errors.New("root")
	if !errors.Is(&Error{Message: "x", Cause: cause}, cause) {
		t.Fatal("unwrap")
	}
}

func TestSetCountsFilter(t *testing.T) {
	a, b, c := New("a", nil, nil), New("b", nil, nil), New("c", nil, nil)
	b.Fail(nil)
	c.Drop()
	s := NewSet(a, b, c)
	if got := s.Counts(); got != (Counts{1, 1, 1}) {
		t.Fatalf("%+v", got)
	}
	if s.Filter(StatusError).Len() != 1 || s.Filter(StatusError).At(0) != b {
		t.Fatal("filter")
	}
	if len(s.Docs()) != 3 {
		t.Fatal("docs")
	}
}

func makeSet(t testing.TB, sp *Spool, n int) *Set {
	s := NewSet()
	for i := 0; i < n; i++ {
		p, err := sp.FromBytes([]byte(fmt.Sprintf("doc-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		s.Add(New(fmt.Sprint(i), nil, p))
	}
	return s
}

func TestApply(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name  string
		n     int
		conc  int
		fn    Func
		want  Counts
		check func(t *testing.T, out *Set)
	}{
		{"empty set", 0, 4, func(_ context.Context, d *Document) (*Document, error) { return d, nil }, Counts{}, nil},
		{"all ok in order", 100, 8, func(_ context.Context, d *Document) (*Document, error) {
			time.Sleep(time.Duration(100-len(d.ID)) * time.Microsecond)
			d.Props["seen"] = "1"
			return d, nil
		}, Counts{OK: 100}, func(t *testing.T, out *Set) {
			for i, d := range out.Docs() {
				if d.ID != fmt.Sprint(i) || d.Props["seen"] != "1" {
					t.Fatalf("order/props broken at %d", i)
				}
			}
		}},
		{"error isolation", 10, 3, func(_ context.Context, d *Document) (*Document, error) {
			if d.ID == "3" || d.ID == "7" {
				return nil, boom
			}
			return d, nil
		}, Counts{OK: 8, Error: 2}, func(t *testing.T, out *Set) {
			if !errors.Is(out.At(3).Err(), boom) || out.At(3).Err().Step != "s" {
				t.Fatalf("detail: %v", out.At(3).Err())
			}
		}},
		{"drop", 5, 2, func(_ context.Context, d *Document) (*Document, error) {
			if d.ID == "1" {
				return nil, nil
			}
			return d, nil
		}, Counts{OK: 4, Dropped: 1}, nil},
		{"panic recovered", 4, 2, func(_ context.Context, d *Document) (*Document, error) {
			if d.ID == "2" {
				panic("kaboom")
			}
			return d, nil
		}, Counts{OK: 3, Error: 1}, func(t *testing.T, out *Set) {
			if out.At(2).Err().Code != "panic" || out.At(2).Err().Message != "kaboom" {
				t.Fatal("panic detail")
			}
		}},
		{"typed error keeps code", 1, 1, func(_ context.Context, d *Document) (*Document, error) {
			return nil, fmt.Errorf("wrap: %w", &Error{Code: "HTTP_500", Message: "srv"})
		}, Counts{Error: 1}, func(t *testing.T, out *Set) {
			if out.At(0).Err().Code != "HTTP_500" {
				t.Fatal("code lost")
			}
		}},
		{"replacement doc releases input", 3, 1, func(_ context.Context, d *Document) (*Document, error) {
			return New("n"+d.ID, nil, nil), nil
		}, Counts{OK: 3}, func(t *testing.T, out *Set) {
			if out.At(0).ID != "n0" {
				t.Fatal("replacement missing")
			}
		}},
		{"zero concurrency defaults", 5, 0, func(_ context.Context, d *Document) (*Document, error) { return d, nil }, Counts{OK: 5}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sp := NewSpool(SpoolConfig{Dir: t.TempDir(), DocThreshold: 1})
			in := makeSet(t, sp, tc.n)
			out, err := Apply(context.Background(), in, Options{Concurrency: tc.conc, Step: "s"}, tc.fn)
			if err != nil {
				t.Fatal(err)
			}
			if got := out.Counts(); got != tc.want {
				t.Fatalf("counts %+v want %+v", got, tc.want)
			}
			if tc.check != nil {
				tc.check(t, out)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			if spillFiles(t, sp.dir) != 0 {
				t.Fatal("spill files leaked")
			}
		})
	}
}

func TestApplySkipsNonOK(t *testing.T) {
	a, b := New("a", nil, nil), New("b", nil, nil)
	a.Fail(&Error{Message: "pre"})
	var calls atomic.Int32
	out, _ := Apply(context.Background(), NewSet(a, b), Options{}, func(_ context.Context, d *Document) (*Document, error) {
		calls.Add(1)
		return d, nil
	})
	if calls.Load() != 1 || out.At(0).Err().Message != "pre" {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestApplyBoundedConcurrency(t *testing.T) {
	const limit = 3
	var cur, peak atomic.Int32
	in := NewSet()
	for i := 0; i < 30; i++ {
		in.Add(New(fmt.Sprint(i), nil, nil))
	}
	_, err := Apply(context.Background(), in, Options{Concurrency: limit}, func(_ context.Context, d *Document) (*Document, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		cur.Add(-1)
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() > limit || peak.Load() < 2 {
		t.Fatalf("peak=%d", peak.Load())
	}
}

func TestApplyCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := NewSet()
	for i := 0; i < 20; i++ {
		in.Add(New(fmt.Sprint(i), nil, nil))
	}
	out, err := Apply(ctx, in, Options{Concurrency: 1, Step: "s"}, func(_ context.Context, d *Document) (*Document, error) {
		if d.ID == "2" {
			cancel()
		}
		return d, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if out.Len() != 20 {
		t.Fatal("len")
	}
	c := out.Counts()
	if c.Error == 0 || c.OK < 3 || c.OK+c.Error != 20 {
		t.Fatalf("%+v", c)
	}
	if !errors.Is(out.At(19).Err(), context.Canceled) {
		t.Fatal("cancel cause")
	}
}

func TestApplyLargeSpillPipeline(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(SpoolConfig{Dir: dir, DocThreshold: 1 << 10, TotalBudget: 4 << 10})
	in := NewSet()
	for i := 0; i < 8; i++ {
		p, err := sp.NewPayload(io.LimitReader(zeroReader{}, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		in.Add(New(fmt.Sprint(i), nil, p))
	}
	// Transform: stream payload through a counter into a new spilled payload.
	out, err := Apply(context.Background(), in, Options{Concurrency: 4}, func(_ context.Context, d *Document) (*Document, error) {
		rc, err := d.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		np, err := sp.NewPayload(rc)
		if err != nil {
			return nil, err
		}
		return New(d.ID, d.Props, np), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Counts().OK != 8 || spillFiles(t, dir) != 8 {
		t.Fatalf("%+v files=%d (inputs should be released)", out.Counts(), spillFiles(t, dir))
	}
	_ = out.Close()
	if spillFiles(t, dir) != 0 {
		t.Fatal("leak")
	}
}
