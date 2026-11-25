// SPDX-License-Identifier: Apache-2.0

package sftp

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type outputBudget interface {
	OutputBytesRemaining() (int64, bool)
	RejectOutputLimit() error
}

var errDirectoryBudget = errors.New("sftp: directory response exceeds the wire budget")

type directoryLimit struct {
	remaining int64 // used only by the SFTP client reader goroutine
	exceeded  atomic.Bool
}

type directoryReader struct {
	input io.Reader
	limit atomic.Pointer[directoryLimit]
}

func (r *directoryReader) Read(p []byte) (int, error) {
	limit := r.limit.Load()
	if limit != nil {
		if limit.remaining <= 0 {
			limit.exceeded.Store(true)
			return 0, errDirectoryBudget
		}
		p = p[:min(int64(len(p)), limit.remaining)]
	}
	n, err := r.input.Read(p)
	// The reader may already be waiting for the next packet header when a
	// listing starts. Charge those bytes too, once the header arrives.
	if current := r.limit.Load(); current != nil {
		current.remaining -= int64(n)
	}
	return n, err
}

type directoryWriter struct {
	io.Writer
	io.Closer
}

func newDirectoryClient(c *ssh.Client) (*pkgsftp.Client, *directoryReader, error) {
	channel, err := c.NewSession()
	if err != nil {
		return nil, nil, err
	}
	input, err := channel.StdinPipe()
	if err != nil {
		_ = channel.Close()
		return nil, nil, err
	}
	output, err := channel.StdoutPipe()
	if err != nil {
		_ = channel.Close()
		return nil, nil, err
	}
	errOut, err := channel.StderrPipe()
	if err != nil {
		_ = channel.Close()
		return nil, nil, err
	}
	if err := channel.RequestSubsystem("sftp"); err != nil {
		_ = channel.Close()
		return nil, nil, err
	}
	go func() { _, _ = io.Copy(io.Discard, errOut) }()
	reader := &directoryReader{input: output}
	client, err := pkgsftp.NewClientPipe(reader, directoryWriter{Writer: input, Closer: channel})
	return client, reader, err
}

// The SFTP library accumulates directory entries before returning them. Bound
// the bytes it can read, including longnames and metadata that won't appear in
// JSON. The connector separately enforces the exact encoded output limit.
func (s *session) readBoundedDir(ctx context.Context, path string, remaining int64) ([]os.FileInfo, error) {
	limit := &directoryLimit{remaining: 2*remaining + 64*1024}
	s.directory.limit.Store(limit)
	defer s.directory.limit.Store(nil)
	entries, err := s.fs.ReadDirContext(ctx, path)
	if limit.exceeded.Load() {
		return nil, errDirectoryBudget
	}
	return entries, err
}
