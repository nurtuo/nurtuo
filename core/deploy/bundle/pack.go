// SPDX-License-Identifier: AGPL-3.0-only

package bundle

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"time"
)

// Pack builds a signed bundle. Identical Input and key yield byte-identical
// output (Ed25519 signatures are deterministic).
func Pack(in Input, priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: bad private key", ErrInvalid)
	}
	if !nameRe.MatchString(in.Name) {
		return nil, fmt.Errorf("%w: bad bundle name %q", ErrInvalid, in.Name)
	}
	m := Manifest{Format: FormatVersion, Name: in.Name, CreatedAt: in.CreatedAt.UTC().Format(time.RFC3339), Connectors: append([]ConnectorRef{}, in.Connectors...)}
	files := map[string][]byte{}
	seen := map[string]bool{}
	for _, c := range in.Components {
		if !nameRe.MatchString(c.Kind) || !nameRe.MatchString(c.ID) || c.Revision < 1 {
			return nil, fmt.Errorf("%w: bad component %q/%q@%d", ErrInvalid, c.Kind, c.ID, c.Revision)
		}
		doc, err := canonicalJSON(c.Document)
		if err != nil {
			return nil, err
		}
		e := ComponentEntry{Kind: c.Kind, ID: c.ID, Revision: c.Revision}
		e.Path = componentPath(e)
		if seen[e.Path] {
			return nil, fmt.Errorf("%w: duplicate component %s", ErrInvalid, e.Path)
		}
		seen[e.Path] = true
		e.SHA256, e.Size = sum(doc), int64(len(doc))
		files[e.Path] = doc
		m.Components = append(m.Components, e)
	}
	cfgRaw := in.Config
	if len(cfgRaw) == 0 {
		cfgRaw = json.RawMessage("{}")
	}
	cfg, err := canonicalJSON(cfgRaw)
	if err != nil {
		return nil, err
	}
	m.Config = FileRef{Path: ConfigPath, SHA256: sum(cfg), Size: int64(len(cfg))}
	files[ConfigPath] = cfg
	if m.Components == nil {
		m.Components = []ComponentEntry{}
	}
	for _, c := range m.Connectors {
		if !nameRe.MatchString(c.ID) || c.Version == "" {
			return nil, fmt.Errorf("%w: bad connector ref %q@%q", ErrInvalid, c.ID, c.Version)
		}
	}
	sortManifest(&m)
	for i := 1; i < len(m.Connectors); i++ {
		if a, b := m.Connectors[i-1], m.Connectors[i]; a.ID == b.ID && a.Version == b.Version {
			return nil, fmt.Errorf("%w: duplicate connector ref %s@%s", ErrInvalid, a.ID, a.Version)
		}
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	files[ManifestPath] = mb
	sig, err := encodeSignature(priv, mb)
	if err != nil {
		return nil, err
	}
	files[SignaturePath] = sig

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range expectedEntries(&m) {
		data := files[name]
		h := &tar.Header{Name: name, Size: int64(len(data)), Mode: 0o644, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > MaxBundleSize {
		return nil, fmt.Errorf("%w: bundle larger than %d bytes", ErrInvalid, MaxBundleSize)
	}
	return buf.Bytes(), nil
}
