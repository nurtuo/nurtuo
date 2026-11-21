// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
)

// HTTP document property names set by the listener (and read back from Return documents).
const (
	PropMethod         = "http.request.method" // request metadata lives under http.request.* so it never doubles as an HTTP-connector override (http.method, http.path, http.header.*, http.query.*)
	PropPath           = "http.request.path"
	PropHeaderPrefix   = "http.request.header."  // request headers, lowercased
	PropQueryPrefix    = "http.request.query."   // first value of each query parameter
	PropStatus         = "http.status"           // response status set by the flow (default 200)
	PropRespHeaderPref = "http.response.header." // response headers set by the flow
)

// HTTPConfig configures an HTTP listener.
type HTTPConfig struct {
	Addr         string        // listen address; default "127.0.0.1:0"
	Path         string        // exact route path (required)
	Method       string        // default POST
	MaxBodyBytes int64         // default 1 MiB
	Timeout      time.Duration // per-request flow timeout; 0 = none
	Spool        *docs.Spool   // payload spool; default in-memory-first spool
}

type httpListener struct {
	cfg   HTTPConfig
	spool *docs.Spool

	mu      sync.Mutex
	state   State
	srv     *http.Server
	addr    string
	handler Handler
	done    chan struct{}

	paused atomic.Bool
	recv   atomic.Int64
	failed atomic.Int64
	seq    atomic.Int64
}

// NewHTTP returns an HTTP listener.
func NewHTTP(cfg HTTPConfig) (Listener, error) {
	if !strings.HasPrefix(cfg.Path, "/") {
		return nil, errors.New("listener: http path must start with /")
	}
	if cfg.Method == "" {
		cfg.Method = http.MethodPost
	}
	cfg.Method = strings.ToUpper(cfg.Method)
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	sp := cfg.Spool
	if sp == nil {
		sp = docs.NewSpool(docs.SpoolConfig{})
	}
	return &httpListener{cfg: cfg, spool: sp, state: StateStopped}, nil
}

func (l *httpListener) Start(_ context.Context, h Handler) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != StateStopped {
		return ErrState
	}
	ln, err := net.Listen("tcp", l.cfg.Addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(l.cfg.Path, l.serve)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	l.handler, l.addr, l.done = h, ln.Addr().String(), make(chan struct{})
	l.paused.Store(false)
	l.state = StateRunning
	srv, done := l.srv, l.done
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	return nil
}

func (l *httpListener) Stop(ctx context.Context) error {
	l.mu.Lock()
	if l.state == StateStopped {
		l.mu.Unlock()
		return nil
	}
	l.state = StateStopped
	srv, done := l.srv, l.done
	l.mu.Unlock()
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	<-done
	return err
}

func (l *httpListener) Pause() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case StatePaused:
		return nil
	case StateRunning:
		l.state = StatePaused
		l.paused.Store(true)
		return nil
	}
	return ErrState
}

func (l *httpListener) Resume() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case StateRunning:
		return nil
	case StatePaused:
		l.state = StateRunning
		l.paused.Store(false)
		return nil
	}
	return ErrState
}

func (l *httpListener) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Status{State: l.state, Addr: l.addr, Received: l.recv.Load(), Failed: l.failed.Load()}
}

func (l *httpListener) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != l.cfg.Method {
		w.Header().Set("Allow", l.cfg.Method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if l.paused.Load() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "listener paused", http.StatusServiceUnavailable)
		return
	}
	payload, err := l.spool.NewPayload(http.MaxBytesReader(w, r.Body, l.cfg.MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	props := map[string]string{PropMethod: r.Method, PropPath: r.URL.Path}
	for k, v := range r.Header {
		if len(v) > 0 {
			props[PropHeaderPrefix+strings.ToLower(k)] = strings.Join(v, ",")
		}
	}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			props[PropQueryPrefix+k] = v[0]
		}
	}
	doc := docs.New(fmt.Sprintf("http-%d", l.seq.Add(1)), props, payload)
	l.recv.Add(1)

	ctx := r.Context()
	if l.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.cfg.Timeout)
		defer cancel()
	}
	l.mu.Lock()
	h := l.handler
	l.mu.Unlock()
	res, err := h(ctx, docs.NewSet(doc))
	if res != nil {
		defer closeResult(res)
	}
	if err != nil {
		l.failed.Add(1)
		code := http.StatusInternalServerError
		if errors.Is(err, context.DeadlineExceeded) {
			code = http.StatusGatewayTimeout
		}
		http.Error(w, "flow failed", code)
		return
	}
	if res.Failed != nil && res.Failed.Len() > 0 {
		l.failed.Add(1)
		code, msg := http.StatusInternalServerError, "flow failed"
		if e := res.Failed.At(0).Err(); e != nil {
			// A numeric error code 400..599 lets a flow choose the client-visible status.
			if n, perr := strconv.Atoi(e.Code); perr == nil && n >= 400 && n <= 599 {
				code, msg = n, e.Message
			}
		}
		http.Error(w, msg, code)
		return
	}
	if res.Stop != nil && res.Stop.Status >= 100 && res.Stop.Status <= 599 {
		http.Error(w, res.Stop.Message, res.Stop.Status)
		return
	}
	writeReturned(w, res.Returned)
}

func writeReturned(w http.ResponseWriter, ret *docs.Set) {
	if ret == nil || ret.Len() == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusOK
	first := ret.At(0)
	if s, err := strconv.Atoi(first.Props[PropStatus]); err == nil && s >= 100 && s <= 599 {
		status = s
	}
	for k, v := range first.Props {
		if name, ok := strings.CutPrefix(k, PropRespHeaderPref); ok {
			w.Header().Set(name, v)
		}
	}
	if ret.Len() == 1 {
		b, err := readDoc(first)
		if err != nil {
			http.Error(w, "cannot read response", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}
	// Several documents: a JSON array; valid JSON bodies are embedded, others become strings.
	arr := make([]json.RawMessage, 0, ret.Len())
	for _, d := range ret.Docs() {
		b, err := readDoc(d)
		if err != nil {
			http.Error(w, "cannot read response", http.StatusInternalServerError)
			return
		}
		if json.Valid(b) {
			arr = append(arr, b)
		} else {
			s, _ := json.Marshal(string(b))
			arr = append(arr, s)
		}
	}
	out, _ := json.Marshal(arr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func readDoc(d *docs.Document) ([]byte, error) {
	rc, err := d.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}
