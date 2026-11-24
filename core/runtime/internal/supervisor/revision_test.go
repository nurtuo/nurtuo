// SPDX-License-Identifier: AGPL-3.0-only

package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAppliedRevisionSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), RevisionFile)
	fd := newFakeDeployer()
	s, err := NewPersistent(fd, path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Revision() != 0 {
		t.Fatalf("fresh revision = %d", s.Revision())
	}
	if err := s.Reconcile(ctx, 5, []Deployment{{ID: "a", BundleDigest: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	fd.failOn["b"] = errors.New("boom")
	if err := s.Reconcile(ctx, 6, []Deployment{{ID: "a", BundleDigest: "x"}, {ID: "b", BundleDigest: "y"}}, nil); err == nil {
		t.Fatal("expected failure")
	}
	if s.Revision() != 5 {
		t.Fatalf("revision advanced after failed reconcile: %d", s.Revision())
	}
	s2, err := NewPersistent(newFakeDeployer(), path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Revision() != 5 {
		t.Fatalf("revision after restart = %d, want 5", s2.Revision())
	}
	if got := (&Handler{S: s2}).Revision(); got != 0 {
		t.Fatalf("Hello revision after restart = %d, want 0", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm()&0o077 != 0 && !isWindows() {
		t.Fatalf("revision file mode: %v %v", fi, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestRevisionFileCorruptFailsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), RevisionFile)
	for _, content := range []string{"", "abc", "-1", "12 34"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPersistent(newFakeDeployer(), path); err == nil {
			t.Fatalf("content %q: expected error, got nil", content)
		}
	}
}

func TestPersistFailureIsReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, RevisionFile)
	s, err := NewPersistent(newFakeDeployer(), path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{S: s}
	check := func(revision uint64) {
		t.Helper()
		if got := s.Revision(); got != revision {
			t.Errorf("applied revision = %d, want %d", got, revision)
		}
		// The gateway uses Handler.Revision to populate Hello.DesiredStateRevision.
		if got := h.Revision(); got != revision {
			t.Errorf("Hello revision = %d, want %d", got, revision)
		}
	}
	reconcile := func(rev uint64) error { return s.Reconcile(t.Context(), rev, nil, nil) }
	if err := reconcile(1); err == nil {
		t.Fatal("expected persist error on first reconcile")
	}
	check(0)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(1); err != nil {
		t.Fatal(err)
	}
	check(1)

	if err := os.Rename(dir, dir+"-backup"); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(2); err == nil {
		t.Fatal("expected persist error on later reconcile")
	}
	check(1)
	if err := os.Rename(dir+"-backup", dir); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(2); err != nil {
		t.Fatal(err)
	}
	check(2)
	if got, err := LoadRevision(path); err != nil || got != 2 {
		t.Fatalf("saved revision = %d, error = %v", got, err)
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }

func TestRevisionDirectorySyncFailure(t *testing.T) {
	if isWindows() {
		t.Skip("directory sync is skipped on Windows")
	}
	dir := t.TempDir()
	s, err := NewPersistent(newFakeDeployer(), filepath.Join(dir, RevisionFile))
	if err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("directory sync failed")
	s.syncRevDir = func(path string) error {
		if path != dir {
			t.Errorf("sync directory = %q, want %q", path, dir)
		}
		return syncErr
	}
	if err := s.Reconcile(t.Context(), 5, nil, nil); !errors.Is(err, syncErr) {
		t.Fatalf("reconcile error = %v, want %v", err, syncErr)
	}
	h := &Handler{S: s}
	if got := h.Revision(); got != 0 {
		t.Fatalf("Hello revision after sync failure = %d, want 0", got)
	}
	s.syncRevDir = syncRevisionDir
	if err := s.Reconcile(t.Context(), 5, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := h.Revision(); got != 5 {
		t.Fatalf("Hello revision after retry = %d, want 5", got)
	}
}
