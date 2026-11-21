// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Dir keeps one file per secret in a directory. On POSIX the directory and the files must not be
// accessible by group or other; symlinks and non-regular files are refused.
type Dir struct{ Path string }

// NewDir returns a dir provider for path.
func NewDir(path string) *Dir { return &Dir{Path: path} }

func (d *Dir) checkDir() error {
	fi, err := os.Lstat(d.Path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("secrets dir %s is not a directory", d.Path)
	}
	return checkPerm(d.Path, fi)
}

func (d *Dir) file(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return filepath.Join(d.Path, name), nil
}

func (d *Dir) Get(_ context.Context, name string) (Secret, error) {
	p, err := d.file(name)
	if err != nil {
		return Secret{}, err
	}
	if err := d.checkDir(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Secret{}, notFound(name)
		}
		return Secret{}, err
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Secret{}, notFound(name)
	}
	if err != nil {
		return Secret{}, fmt.Errorf("secret %q: %w", name, err)
	}
	if !fi.Mode().IsRegular() {
		return Secret{}, fmt.Errorf("secret %q: %s is not a regular file", name, p)
	}
	if err := checkPerm(p, fi); err != nil {
		return Secret{}, fmt.Errorf("secret %q: %w", name, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Secret{}, fmt.Errorf("secret %q: %w", name, err)
	}
	v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if v == "" {
		return Secret{}, notFound(name)
	}
	return New(v), nil
}

func (d *Dir) Set(_ context.Context, name string, v Secret) error {
	p, err := d.file(name)
	if err != nil {
		return err
	}
	if v.Reveal() == "" {
		return errors.New("secret value is empty")
	}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	if err := d.checkDir(); err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(v.Reveal()))
}

func (d *Dir) Delete(_ context.Context, name string) error {
	p, err := d.file(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return notFound(name)
		}
		return err
	}
	return nil
}

func (d *Dir) List(context.Context) ([]string, error) {
	ents, err := os.ReadDir(d.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && ValidName(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	return sorted(out), nil
}

// writeFileAtomic writes data to a 0600 temp file in the same directory and renames it over path.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := chmodPrivate(tmp); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
