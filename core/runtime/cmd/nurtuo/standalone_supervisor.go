// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ChildSpec describes a supervised child process.
type ChildSpec struct {
	Name        string
	Command     []string // argv
	Env         []string // appended to os.Environ()
	Dir         string
	Log         io.Writer     // captured stdout+stderr, line-prefixed with Name
	MinBackoff  time.Duration // default 500ms
	MaxBackoff  time.Duration // default 30s
	StableFor   time.Duration // run time after which backoff resets; default 30s
	MaxRestarts int           // 0 = unlimited (consecutive unstable restarts)
	StopGrace   time.Duration // wait after interrupt before kill; default 5s
}

// Child supervises one process with restart and exponential backoff.
type Child struct {
	spec ChildSpec

	mu      sync.Mutex
	starts  int
	running bool
}

func NewChild(spec ChildSpec) *Child {
	if spec.MinBackoff == 0 {
		spec.MinBackoff = 500 * time.Millisecond
	}
	if spec.MaxBackoff == 0 {
		spec.MaxBackoff = 30 * time.Second
	}
	if spec.StableFor == 0 {
		spec.StableFor = 30 * time.Second
	}
	if spec.StopGrace == 0 {
		spec.StopGrace = 5 * time.Second
	}
	return &Child{spec: spec}
}

// Starts reports how many times the process has been launched.
func (c *Child) Starts() int { c.mu.Lock(); defer c.mu.Unlock(); return c.starts }

// Run starts the child and restarts it on exit until ctx is cancelled (returns nil after the
// child has been stopped) or MaxRestarts consecutive unstable restarts are exceeded.
func (c *Child) Run(ctx context.Context) error {
	if len(c.spec.Command) == 0 {
		return errors.New("supervisor: empty command")
	}
	backoff := c.spec.MinBackoff
	unstable := 0
	for {
		began := time.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(began) >= c.spec.StableFor {
			backoff, unstable = c.spec.MinBackoff, 0
		}
		unstable++
		if c.spec.MaxRestarts > 0 && unstable > c.spec.MaxRestarts {
			return fmt.Errorf("supervisor: %s exceeded %d restarts (last: %v)", c.spec.Name, c.spec.MaxRestarts, err)
		}
		c.logf("exited (%v); restarting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > c.spec.MaxBackoff {
			backoff = c.spec.MaxBackoff
		}
	}
}

func (c *Child) runOnce(ctx context.Context) error {
	cmd := exec.Command(c.spec.Command[0], c.spec.Command[1:]...)
	cmd.Dir = c.spec.Dir
	cmd.Env = append(os.Environ(), c.spec.Env...)
	pw := newPrefixWriter(c.spec.Log, c.spec.Name)
	defer pw.Close()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	c.mu.Lock()
	c.starts++
	c.running = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock() }()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Interrupt is unsupported on Windows (returns an error); fall through to kill.
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case werr := <-done: // exit status superseded by ctx.Err()
			_ = werr
		case <-time.After(c.spec.StopGrace):
			_ = cmd.Process.Kill()
			if werr := <-done; werr != nil {
				c.logf("process exited after stop: %v", werr)
			}
		}
		return ctx.Err()
	}
}

func (c *Child) logf(format string, a ...any) {
	if c.spec.Log != nil {
		_, _ = fmt.Fprintf(c.spec.Log, "[%s] supervisor: %s\n", c.spec.Name, fmt.Sprintf(format, a...))
	}
}

// HTTPProbe returns a health probe: any response with status < 500 counts as healthy.
func HTTPProbe(url string) func(context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
		}
		return nil
	}
}

// WaitHealthy polls probe every interval until it succeeds or timeout/ctx expires.
func WaitHealthy(ctx context.Context, probe func(context.Context) error, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		if last = probe(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy after %s: %w", timeout, last)
		case <-time.After(interval):
		}
	}
}

// prefixWriter line-buffers output and writes "[name] line" to w (nil w discards).
type prefixWriter struct {
	pr *io.PipeReader
	pw *io.PipeWriter
	wg sync.WaitGroup
}

func newPrefixWriter(w io.Writer, name string) *prefixWriter {
	p := &prefixWriter{}
	p.pr, p.pw = io.Pipe()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		sc := bufio.NewScanner(p.pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if w != nil {
				_, _ = fmt.Fprintf(w, "[%s] %s\n", name, sc.Text())
			}
		}
		_, _ = io.Copy(io.Discard, p.pr)
	}()
	return p
}

func (p *prefixWriter) Write(b []byte) (int, error) { return p.pw.Write(b) }
func (p *prefixWriter) Close()                      { _ = p.pw.Close(); p.wg.Wait() }
