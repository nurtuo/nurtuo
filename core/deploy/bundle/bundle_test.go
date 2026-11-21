// SPDX-License-Identifier: AGPL-3.0-only

package bundle

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

type entry struct {
	name string
	data []byte
}

func sampleInput() Input {
	return Input{
		Name:      "orders-prod",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Components: []Component{
			{Kind: "flow", ID: "sync-orders", Revision: 3, Document: json.RawMessage(`{"nodes":[],"b":1,"a":2}`)},
			{Kind: "mapping", ID: "order-to-erp", Revision: 1, Document: json.RawMessage(`{"x":"y"}`)},
		},
		Config:     json.RawMessage(`{"env":"prod"}`),
		Connectors: []ConnectorRef{{ID: "http", Version: "1.2.0", Digest: "sha256:abc"}, {ID: "postgres", Version: "0.9.1"}},
	}
}

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func readEntries(t *testing.T, b []byte) []entry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var out []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		d, _ := io.ReadAll(tr)
		out = append(out, entry{h.Name, d})
	}
}

func writeEntries(t *testing.T, es []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Size: int64(len(e.data)), Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(e.data)
	}
	_ = tw.Close()
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	data, err := Pack(sampleInput(), priv)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Verify(data, pub)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Manifest.Components) != 2 || b.Manifest.Components[0].ID != "sync-orders" || len(b.Manifest.Connectors) != 2 {
		t.Fatalf("unexpected manifest: %+v", b.Manifest)
	}
	if got := string(b.Files["components/flow/sync-orders/3.json"]); got != `{"a":2,"b":1,"nodes":[]}` {
		t.Fatalf("document not canonical: %s", got)
	}
}

func TestReproducible(t *testing.T) {
	_, priv := mustKey(t)
	a, err := Pack(sampleInput(), priv)
	if err != nil {
		t.Fatal(err)
	}
	// Same logical input with components/connectors in another order and
	// differently formatted JSON must still produce identical bytes.
	in := sampleInput()
	in.Components[0], in.Components[1] = in.Components[1], in.Components[0]
	in.Connectors[0], in.Connectors[1] = in.Connectors[1], in.Connectors[0]
	in.Components[1].Document = json.RawMessage("{ \"a\": 2,\n \"nodes\": [], \"b\": 1 }")
	in.CreatedAt = in.CreatedAt.In(time.FixedZone("x", 3600)).Add(0)
	b, err := Pack(in, priv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("bundles differ for equivalent input")
	}
	c, _ := Pack(sampleInput(), priv)
	if !bytes.Equal(a, c) {
		t.Fatal("repeat pack differs")
	}
}

func TestModifiedFileRejected(t *testing.T) {
	pub, priv := mustKey(t)
	data, _ := Pack(sampleInput(), priv)
	es := readEntries(t, data)
	for i := range es {
		if es[i].name == "components/mapping/order-to-erp/1.json" {
			es[i].data = []byte(`{"x":"evil"}`)
		}
	}
	if _, err := Verify(writeEntries(t, es), pub); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v", err)
	}
}

func TestModifiedManifestRejected(t *testing.T) {
	pub, priv := mustKey(t)
	data, _ := Pack(sampleInput(), priv)
	es := readEntries(t, data)
	es[0].data = bytes.Replace(es[0].data, []byte("orders-prod"), []byte("orders-evil"), 1)
	if _, err := Verify(writeEntries(t, es), pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	_, priv := mustKey(t)
	other, _ := mustKey(t)
	data, _ := Pack(sampleInput(), priv)
	if _, err := Verify(data, other); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestMissingSignatureRejected(t *testing.T) {
	pub, priv := mustKey(t)
	data, _ := Pack(sampleInput(), priv)
	es := readEntries(t, data)
	es = es[:len(es)-1]
	if _, err := Verify(writeEntries(t, es), pub); !errors.Is(err, ErrMissingSig) {
		t.Fatalf("want ErrMissingSig, got %v", err)
	}
}

func TestReorderedEntriesRejected(t *testing.T) {
	pub, priv := mustKey(t)
	data, _ := Pack(sampleInput(), priv)
	es := readEntries(t, data)
	es[2], es[3] = es[3], es[2] // swap two component files; signature and hashes still valid
	if _, err := Verify(writeEntries(t, es), pub); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("want ErrNonCanonical, got %v", err)
	}
}

func TestExtraAndDuplicateEntriesRejected(t *testing.T) {
	pub, priv := mustKey(t)
	data, _ := Pack(sampleInput(), priv)
	es := readEntries(t, data)
	extra := append(append([]entry{}, es[:len(es)-1]...), entry{"components/flow/evil/1.json", []byte(`{}`)}, es[len(es)-1])
	if _, err := Verify(writeEntries(t, extra), pub); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("extra entry: got %v", err)
	}
	dup := append(append([]entry{}, es...), es[1])
	if _, err := Verify(writeEntries(t, dup), pub); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate entry: got %v", err)
	}
}

func TestGarbageRejected(t *testing.T) {
	pub, _ := mustKey(t)
	if _, err := Verify([]byte("not a tar"), pub); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestPackValidation(t *testing.T) {
	_, priv := mustKey(t)
	in := sampleInput()
	in.Components[0].ID = "../escape"
	if _, err := Pack(in, priv); !errors.Is(err, ErrInvalid) {
		t.Fatalf("path traversal id accepted: %v", err)
	}
	in = sampleInput()
	in.Components = append(in.Components, in.Components[0])
	if _, err := Pack(in, priv); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func TestKeyEncoding(t *testing.T) {
	pub, priv := mustKey(t)
	p2, err := DecodePrivateKey(EncodePrivateKey(priv))
	if err != nil || !priv.Equal(p2) {
		t.Fatal("private key round trip failed")
	}
	u2, err := DecodePublicKey(EncodePublicKey(pub))
	if err != nil || !pub.Equal(u2) {
		t.Fatal("public key round trip failed")
	}
}
