// SPDX-License-Identifier: AGPL-3.0-only

package bundle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type signatureDoc struct {
	Alg   string `json:"alg"`
	KeyID string `json:"keyId"`
	Sig   string `json:"sig"`
}

// KeyID is a short fingerprint of a public key.
func KeyID(pub ed25519.PublicKey) string { s := sum(pub); return s[:16] }

// GenerateKey creates an Ed25519 key pair from crypto/rand.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// Keys are hex encoded (private key = 32-byte seed) for easy storage.
func EncodePrivateKey(k ed25519.PrivateKey) string { return hex.EncodeToString(k.Seed()) }
func EncodePublicKey(k ed25519.PublicKey) string   { return hex.EncodeToString(k) }

func DecodePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: bad private key encoding", ErrInvalid)
	}
	return ed25519.NewKeyFromSeed(b), nil
}

func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: bad public key encoding", ErrInvalid)
	}
	return ed25519.PublicKey(b), nil
}

func encodeSignature(priv ed25519.PrivateKey, manifest []byte) ([]byte, error) {
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, append([]byte(sigDomain), manifest...))
	return json.Marshal(signatureDoc{Alg: "ed25519", KeyID: KeyID(pub), Sig: base64.StdEncoding.EncodeToString(sig)})
}

func verifySignature(pub ed25519.PublicKey, manifest, raw []byte) error {
	var d signatureDoc
	if err := json.Unmarshal(raw, &d); err != nil || d.Alg != "ed25519" {
		return fmt.Errorf("%w: malformed signature", ErrBadSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(d.Sig)
	if err != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, append([]byte(sigDomain), manifest...), sig) {
		return ErrBadSignature
	}
	return nil
}
