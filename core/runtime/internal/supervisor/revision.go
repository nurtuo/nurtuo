// SPDX-License-Identifier: AGPL-3.0-only

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// RevisionFile is the file name of the persisted applied revision inside the state directory.
const RevisionFile = "revision"

// LoadRevision reads a revision file. A missing file means revision 0; a corrupt file is an error
// (it must not silently reset the revision to 0).
func LoadRevision(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read revision file %s: %w", path, err)
	}
	rev, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("revision file %s is corrupt: %w", path, err)
	}
	return rev, nil
}

// saveRevision writes the revision atomically: temp file in the same directory, fsync, rename.
func saveRevision(path string, rev uint64, syncDir func(string) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".revision-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if _, err := f.WriteString(strconv.FormatUint(rev, 10) + "\n"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if runtime.GOOS != "windows" {
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("sync revision directory %s: %w", dir, err)
		}
	}
	return nil
}

func syncRevisionDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// NewPersistent is New with the applied revision stored in path: it is loaded now (corrupt file =
// error) and rewritten after every fully successful Reconcile.
func NewPersistent(d Deployer, path string) (*Supervisor, error) {
	rev, err := LoadRevision(path)
	if err != nil {
		return nil, err
	}
	s := New(d)
	s.revision = rev
	s.revPath = path
	s.syncRevDir = syncRevisionDir
	return s, nil
}
