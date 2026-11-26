// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"io"
	"os"

	"golang.org/x/crypto/ssh"
)

func init() {
	// Private control-plane protocol. Credentials enter through stdin only.
	register("_validate-sftp-key", func(args []string, _, _ io.Writer) int {
		if len(args) != 0 {
			return 2
		}
		return validateSFTPKey(os.Stdin)
	})
}

func validateSFTPKey(input io.Reader) int {
	var request struct {
		PrivateKey string
		Passphrase string
	}
	// Two 64 KiB strings, allowing for JSON escaping.
	if err := json.NewDecoder(io.LimitReader(input, 800<<10)).Decode(&request); err != nil {
		return 2
	}
	var err error
	if request.Passphrase != "" {
		_, err = ssh.ParsePrivateKeyWithPassphrase([]byte(request.PrivateKey), []byte(request.Passphrase))
	} else {
		_, err = ssh.ParsePrivateKey([]byte(request.PrivateKey))
	}
	if err != nil {
		return 1
	}
	return 0
}
