// SPDX-License-Identifier: AGPL-3.0-only

// Package bundle implements the deterministic, signed deployment bundle format.
// Flow and component documents are treated as opaque JSON.
package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"
)

const (
	FormatVersion = "nurtuo.bundle/v1"
	ManifestPath  = "manifest.json"
	SignaturePath = "manifest.sig"
	ConfigPath    = "config.json"
	componentsDir = "components/"
	sigDomain     = "nurtuo-bundle-v1\n"
)

var (
	ErrInvalid      = errors.New("bundle: invalid")
	ErrMissingSig   = errors.New("bundle: missing signature")
	ErrBadSignature = errors.New("bundle: signature verification failed")
	ErrHashMismatch = errors.New("bundle: content hash mismatch")
	ErrNonCanonical = errors.New("bundle: entries not in canonical order")
	nameRe          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

// FileRef points at a bundle entry and pins its content.
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ComponentEntry is one component revision (flow, mapping, schema, ...).
type ComponentEntry struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	FileRef
}

// ConnectorRef pins a connector version (and optionally its registry digest).
type ConnectorRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
}

// Manifest is the signed document. Slices are always sorted.
type Manifest struct {
	Format     string           `json:"format"`
	Name       string           `json:"name"`
	CreatedAt  string           `json:"createdAt"` // RFC3339 UTC, supplied by the caller
	Components []ComponentEntry `json:"components"`
	Config     FileRef          `json:"config"`
	Connectors []ConnectorRef   `json:"connectors"`
}

// Component is a packer input.
type Component struct {
	Kind     string
	ID       string
	Revision int
	Document json.RawMessage // opaque JSON
}

// Input is everything the packer needs. No clock or randomness is consulted.
type Input struct {
	Name       string
	CreatedAt  time.Time
	Components []Component
	Config     json.RawMessage // opaque JSON; nil means {}
	Connectors []ConnectorRef
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// canonicalJSON re-encodes opaque JSON with sorted keys and no insignificant
// whitespace, preserving number literals.
func canonicalJSON(raw []byte) ([]byte, error) {
	if err := rejectDuplicateKeys(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: bad json: %v", ErrInvalid, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing json data", ErrInvalid)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// rejectDuplicateKeys walks the first JSON value of dec and fails on any object
// with a repeated key (encoding/json would silently keep the last one). Syntax
// errors are left to the caller's decode.
func rejectDuplicateKeys(dec *json.Decoder) error {
	var walk func() error
	walk = func() error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				if seen[k] {
					return fmt.Errorf("duplicate object key %q", k)
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // closing delimiter
		return err
	}
	if err := walk(); err != nil && err != io.EOF && !isSyntax(err) {
		return err
	}
	return nil
}

func isSyntax(err error) bool {
	var se *json.SyntaxError
	return errors.As(err, &se)
}

func componentPath(c ComponentEntry) string {
	return fmt.Sprintf("%s%s/%s/%d.json", componentsDir, c.Kind, c.ID, c.Revision)
}

func sortManifest(m *Manifest) {
	sort.Slice(m.Components, func(i, j int) bool { return m.Components[i].Path < m.Components[j].Path })
	sort.Slice(m.Connectors, func(i, j int) bool { return connectorLess(m.Connectors[i], m.Connectors[j]) })
}

func connectorLess(a, b ConnectorRef) bool {
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	return a.Version < b.Version
}

// checkManifestShape verifies the invariants Pack establishes: the config path
// is fixed, every component path is derived from its kind/id/revision, and the
// component and connector lists are strictly sorted (hence unique).
func checkManifestShape(m *Manifest) error {
	if m.Config.Path != ConfigPath {
		return fmt.Errorf("%w: config path %q", ErrInvalid, m.Config.Path)
	}
	for i, c := range m.Components {
		if !nameRe.MatchString(c.Kind) || !nameRe.MatchString(c.ID) || c.Revision < 1 || c.Path != componentPath(c) {
			return fmt.Errorf("%w: component %q does not match its kind/id/revision", ErrInvalid, c.Path)
		}
		if i > 0 && m.Components[i-1].Path >= c.Path {
			return fmt.Errorf("%w: components not sorted and unique at %q", ErrNonCanonical, c.Path)
		}
	}
	for i, c := range m.Connectors {
		if i > 0 && !connectorLess(m.Connectors[i-1], c) {
			return fmt.Errorf("%w: connectors not sorted and unique at %q@%q", ErrNonCanonical, c.ID, c.Version)
		}
	}
	return nil
}

// expectedEntries returns the canonical entry order implied by a manifest.
func expectedEntries(m *Manifest) []string {
	out := []string{ManifestPath, m.Config.Path}
	for _, c := range m.Components {
		out = append(out, c.Path)
	}
	return append(out, SignaturePath)
}
