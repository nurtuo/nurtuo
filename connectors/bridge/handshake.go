// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Handshake protocol constants. The normative description is bridges/README.md.
const (
	HandshakePrefix  = "NURTUO-BRIDGE"
	HandshakeVersion = 1

	// ContractV1 is the connector contract package spoken by this launcher.
	ContractV1 = "nurtuo.connector.v1"

	EnvListen     = "NURTUO_BRIDGE_LISTEN"
	EnvContract   = "NURTUO_BRIDGE_CONTRACT"
	EnvToken      = "NURTUO_BRIDGE_TOKEN"
	EnvWatchStdin = "NURTUO_BRIDGE_WATCH_STDIN"

	// TokenMetadataKey is the gRPC metadata key that carries the shared token on every RPC.
	TokenMetadataKey = "x-nurtuo-bridge-token"
)

// Handshake is the parsed handshake line a bridge prints on stdout once it listens:
//
//	NURTUO-BRIDGE|1|<network>|<address>|<contract>|<implementation>
//
// network is "unix" (address is a socket path) or "tcp" (address is a loopback host:port),
// contract is the connector contract package the bridge will speak (one of the launcher's
// accepted contracts), implementation is free text such as "python/0.1.0" (optional).
type Handshake struct {
	Version        int
	Network        string
	Address        string
	Contract       string
	Implementation string
}

// ParseHandshake parses and validates one handshake line.
func ParseHandshake(line string) (Handshake, error) {
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, "|", 6)
	if len(parts) < 5 || parts[0] != HandshakePrefix {
		return Handshake{}, fmt.Errorf("bridge: not a handshake line: %q", truncate(line, 120))
	}
	v, err := strconv.Atoi(parts[1])
	if err != nil {
		return Handshake{}, fmt.Errorf("bridge: bad handshake version %q", parts[1])
	}
	if v != HandshakeVersion {
		return Handshake{}, fmt.Errorf("bridge: unsupported handshake version %d (launcher speaks %d)", v, HandshakeVersion)
	}
	h := Handshake{Version: v, Network: parts[2], Address: parts[3], Contract: parts[4]}
	if len(parts) == 6 {
		h.Implementation = parts[5]
	}
	switch h.Network {
	case "unix":
		if h.Address == "" {
			return Handshake{}, fmt.Errorf("bridge: empty unix socket path in handshake")
		}
	case "tcp":
		host, port, err := net.SplitHostPort(h.Address)
		if err != nil {
			return Handshake{}, fmt.Errorf("bridge: bad tcp address %q: %w", h.Address, err)
		}
		if p, perr := strconv.Atoi(port); perr != nil || p <= 0 || p > 65535 {
			return Handshake{}, fmt.Errorf("bridge: bad tcp port in %q", h.Address)
		}
		// A bridge must never ask the runtime to talk to a remote host.
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return Handshake{}, fmt.Errorf("bridge: tcp address %q is not loopback", h.Address)
		}
	default:
		return Handshake{}, fmt.Errorf("bridge: unknown network %q in handshake", h.Network)
	}
	return h, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
