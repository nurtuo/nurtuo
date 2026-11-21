// SPDX-License-Identifier: AGPL-3.0-only

package bundle

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
)

const maxEntrySize = 64 << 20

// MaxBundleSize caps the total archive size (and the sum of entry sizes) that
// Verify and Pack accept. It is checked before entries are buffered. Variable so
// deployments with unusually large bundles can raise it at start-up.
var MaxBundleSize int64 = 256 << 20

// Bundle is a verified bundle's contents.
type Bundle struct {
	Manifest Manifest
	Files    map[string][]byte // component and config documents by path
}

// Verify checks signature, canonical entry order and every content hash. The
// signature is checked before the manifest is trusted for anything else.
func Verify(data []byte, pub ed25519.PublicKey) (*Bundle, error) {
	if int64(len(data)) > MaxBundleSize {
		return nil, fmt.Errorf("%w: bundle larger than %d bytes", ErrInvalid, MaxBundleSize)
	}
	br := bytes.NewReader(data)
	tr := tar.NewReader(br)
	var names []string
	var total int64
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxEntrySize {
			return nil, fmt.Errorf("%w: unsupported entry %q", ErrInvalid, h.Name)
		}
		// Checked before the entry is buffered.
		if total += h.Size; total > MaxBundleSize {
			return nil, fmt.Errorf("%w: bundle contents larger than %d bytes", ErrInvalid, MaxBundleSize)
		}
		if _, dup := files[h.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate entry %q", ErrInvalid, h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxEntrySize))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		names = append(names, h.Name)
		files[h.Name] = b
	}
	// The tar reader stops at the end-of-archive marker (two zero blocks) and
	// ignores whatever follows; the container must be non-malleable, so nothing
	// may follow it.
	if br.Len() != 0 {
		return nil, fmt.Errorf("%w: %d bytes after end of archive", ErrNonCanonical, br.Len())
	}
	sig, ok := files[SignaturePath]
	if !ok {
		return nil, ErrMissingSig
	}
	mb, ok := files[ManifestPath]
	if !ok {
		return nil, fmt.Errorf("%w: missing manifest", ErrInvalid)
	}
	if err := verifySignature(pub, mb, sig); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil || m.Format != FormatVersion {
		return nil, fmt.Errorf("%w: bad manifest", ErrInvalid)
	}
	if err := checkManifestShape(&m); err != nil {
		return nil, err
	}
	want := expectedEntries(&m)
	if len(want) != len(names) {
		return nil, fmt.Errorf("%w: expected %d entries, got %d", ErrNonCanonical, len(want), len(names))
	}
	for i := range want {
		if want[i] != names[i] {
			return nil, fmt.Errorf("%w: entry %d is %q, want %q", ErrNonCanonical, i, names[i], want[i])
		}
	}
	refs := []FileRef{m.Config}
	for _, c := range m.Components {
		refs = append(refs, c.FileRef)
	}
	for _, r := range refs {
		b := files[r.Path]
		if sum(b) != r.SHA256 || int64(len(b)) != r.Size {
			return nil, fmt.Errorf("%w: %s", ErrHashMismatch, r.Path)
		}
	}
	delete(files, ManifestPath)
	delete(files, SignaturePath)
	return &Bundle{Manifest: m, Files: files}, nil
}
