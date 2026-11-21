// SPDX-License-Identifier: AGPL-3.0-only

package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestVerifyRejectsTrailingGarbage(t *testing.T) {
	pub, priv := mustKey(t)
	data, err := Pack(sampleInput(), priv)
	if err != nil {
		t.Fatal(err)
	}
	for name, tail := range map[string][]byte{
		"garbage": []byte("trailing garbage"),
		"zeros":   make([]byte, 512),
	} {
		if _, err := Verify(append(append([]byte{}, data...), tail...), pub); err == nil {
			t.Fatalf("%s: Verify accepted trailing data", name)
		}
	}
}

func TestPackRejectsDuplicateConnector(t *testing.T) {
	_, priv := mustKey(t)
	in := sampleInput()
	in.Connectors = append(in.Connectors, in.Connectors[0])
	if _, err := Pack(in, priv); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

// resign builds a validly signed bundle around an arbitrary manifest.
func resign(t *testing.T, m Manifest, files map[string][]byte) ([]byte, []byte) {
	t.Helper()
	pub, priv := mustKey(t)
	mb, _ := json.Marshal(m)
	sig, err := encodeSignature(priv, mb)
	if err != nil {
		t.Fatal(err)
	}
	es := []entry{{ManifestPath, mb}, {m.Config.Path, files[m.Config.Path]}}
	for _, c := range m.Components {
		es = append(es, entry{c.Path, files[c.Path]})
	}
	es = append(es, entry{SignaturePath, sig})
	return writeEntries(t, es), pub
}

func TestVerifyRejectsBadManifestShape(t *testing.T) {
	doc := []byte(`{}`)
	mk := func(c ComponentEntry) Manifest {
		c.SHA256, c.Size = sum(doc), int64(len(doc))
		return Manifest{Format: FormatVersion, Name: "x", Config: FileRef{Path: ConfigPath, SHA256: sum(doc), Size: 2},
			Components: []ComponentEntry{c}, Connectors: []ConnectorRef{}}
	}
	ok := ComponentEntry{Kind: "flow", ID: "a", Revision: 1}
	ok.Path = componentPath(ok)

	wrongPath := ok
	wrongPath.Path = "components/flow/other/1.json"

	cases := map[string]Manifest{"path mismatch": mk(wrongPath)}

	dupConn := mk(ok)
	dupConn.Connectors = []ConnectorRef{{ID: "http", Version: "1"}, {ID: "http", Version: "1"}}
	cases["duplicate connectors"] = dupConn

	unsortedConn := mk(ok)
	unsortedConn.Connectors = []ConnectorRef{{ID: "z", Version: "1"}, {ID: "a", Version: "1"}}
	cases["unsorted connectors"] = unsortedConn

	badCfg := mk(ok)
	badCfg.Config.Path = "evil.json"
	cases["config path"] = badCfg

	for name, m := range cases {
		files := map[string][]byte{m.Config.Path: doc, m.Components[0].Path: doc}
		data, pub := resign(t, m, files)
		if _, err := Verify(data, pub); err == nil {
			t.Errorf("%s: Verify accepted", name)
		}
	}

	// sanity: the well-formed manifest verifies.
	m := mk(ok)
	data, pub := resign(t, m, map[string][]byte{ConfigPath: doc, ok.Path: doc})
	if _, err := Verify(data, pub); err != nil {
		t.Fatalf("well-formed manifest rejected: %v", err)
	}
}

func TestVerifyTotalSizeCap(t *testing.T) {
	old := MaxBundleSize
	MaxBundleSize = 1 << 10
	defer func() { MaxBundleSize = old }()
	pub, priv := mustKey(t)
	in := sampleInput()
	in.Config = json.RawMessage(`{"pad":"` + string(bytes.Repeat([]byte("a"), 4096)) + `"}`)
	data, err := Pack(in, priv)
	if err == nil {
		_, err = Verify(data, pub)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid for oversize bundle", err)
	}
}

func TestCanonicalJSONRejectsDuplicateKeys(t *testing.T) {
	if _, err := canonicalJSON([]byte(`{"a":1,"a":2}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("top-level dup: %v", err)
	}
	if _, err := canonicalJSON([]byte(`{"x":[{"a":1,"a":2}]}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nested dup: %v", err)
	}
	if _, err := canonicalJSON([]byte(`{"a":{"a":1},"b":[{"a":1}]}`)); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
}
