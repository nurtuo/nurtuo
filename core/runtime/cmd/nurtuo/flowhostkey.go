// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type scannedHostKey struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprintSha256"`
}

func flowHostKey(args []string, stdout, stderr io.Writer) int {
	const usage = "Usage: nurtuo flow host-key <host> [--port N] [--timeout 10s] --json"
	fs := flag.NewFlagSet("host-key", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", 22, "")
	timeout := fs.Duration("timeout", 10*time.Second, "")
	asJSON := fs.Bool("json", false, "")
	if len(args) == 0 || fs.Parse(args[1:]) != nil || fs.NArg() != 0 || !*asJSON ||
		strings.TrimSpace(args[0]) == "" || *port < 1 || *port > 65535 || *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	address := net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(args[0], "["), "]"), strconv.Itoa(*port))
	key, err := scanHostKey(ctx, address)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "host key scan: %v\n", err)
		return exitExec
	}
	if err := json.NewEncoder(stdout).Encode(map[string][]scannedHostKey{"keys": {key}}); err != nil {
		return exitExec
	}
	return 0
}

func scanHostKey(ctx context.Context, address string) (scannedHostKey, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return scannedHostKey{}, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	var key scannedHostKey
	scanned := errors.New("host key received")
	// Like the SFTP connector, use the default host key algorithms. Abort before authentication.
	client, _, _, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		HostKeyCallback: func(_ string, _ net.Addr, publicKey ssh.PublicKey) error {
			key = scannedHostKey{Type: publicKey.Type(), Fingerprint: ssh.FingerprintSHA256(publicKey)}
			return scanned
		},
	})
	if errors.Is(err, scanned) {
		return key, nil
	}
	if err == nil {
		_ = client.Close()
		err = errors.New("server did not present a host key")
	}
	return scannedHostKey{}, err
}
