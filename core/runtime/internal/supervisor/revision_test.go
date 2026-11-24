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
	dir := t.TempDir()
	path := filepath.Join(dir, "gone", RevisionFile) // directory does not exist
	s, err := NewPersistent(newFakeDeployer(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(context.Background(), 1, nil, nil); err == nil {
		t.Fatal("expected persist error")
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }
