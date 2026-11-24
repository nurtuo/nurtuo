// SPDX-License-Identifier: AGPL-3.0-only

package bundle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackGoldenMatchesControlPlane(t *testing.T) {
	seed := sha256.Sum256([]byte("nurtuo-golden-bundle-key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	data, err := Pack(Input{
		Name:      "golden",
		CreatedAt: time.Date(2026, 11, 24, 12, 34, 56, 789000000, time.UTC),
		Components: []Component{
			{Kind: "flow", ID: "example", Revision: 1, Document: []byte(`{"z":2,"a":"hello"}`)},
		},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "803cda148d0184722f89de42664a3382dc8f291703d70bd83fe41caacff15d9d"
	if got := sum(data); got != golden {
		t.Fatalf("archive SHA-256 = %s, want %s", got, golden)
	}
	if _, err := Verify(data, key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyControlPlaneUnicodeAndNumbers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the TypeScript packer interop test")
	}
	controlPlane := filepath.Join("..", "..", "control-plane")
	if _, err := os.Stat(filepath.Join(controlPlane, "dist", "bundle.js")); os.IsNotExist(err) {
		t.Skip("build @nurtuo/control-plane to run the TypeScript packer interop test")
	} else if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(keyPath, []byte(EncodePrivateKey(priv)), 0o600); err != nil {
		t.Fatal(err)
	}
	const pack = `
import { loadSigningKey, packBundle } from "./dist/bundle.js";
const archive = packBundle({
  name: "interop",
  createdAt: "2026-11-24T12:34:56Z",
  component: {
    kind: "flow", id: "a".repeat(128), revision: 1,
    document: { "é": "\u2028", "𐀀": 1e21, "\ue000": [1e-7, -2.75, 9007199254740991] },
  },
}, loadSigningKey(process.argv[1]));
process.stdout.write(archive);
`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "--eval", pack, keyPath)
	cmd.Dir = controlPlane
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("TypeScript packer failed: %v", err)
	}
	verified, err := Verify(data, pub)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("{\"é\":\"\u2028\",\"𐀀\":1e+21,\"\ue000\":[1e-7,-2.75,9007199254740991]}")
	path := "components/flow/" + strings.Repeat("a", 128) + "/1.json"
	if got := verified.Files[path]; !bytes.Equal(got, want) {
		t.Fatalf("verified component = %q, want %q", got, want)
	}
}
