// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestValidateSFTPKey(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	const passphrase = "test passphrase"
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(key, "", []byte(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, passphrase string
		key              *pem.Block
		want             int
	}{
		{"plain", "", plain, 0},
		{"encrypted", passphrase, encrypted, 0},
		{"wrong passphrase", "wrong", encrypted, 1},
		{"missing passphrase", "", encrypted, 1},
		{"unexpected passphrase", passphrase, plain, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := json.Marshal(map[string]string{
				"privateKey": string(pem.EncodeToMemory(tc.key)),
				"passphrase": tc.passphrase,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := validateSFTPKey(bytes.NewReader(request)); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}
