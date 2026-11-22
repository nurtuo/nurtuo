// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net"
	"net/http"
	"strings"

	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// Defaults for HTTPAuth.
const (
	DefaultAPIKeyHeader = "X-API-Key"
	DefaultHMACHeader   = "X-Signature"
)

// HTTPAuth authenticates requests to an HTTP listener. Secret holds the resolved shared secret
// (never log it). Type is flow.AuthBearer, flow.AuthAPIKey or flow.AuthHMAC.
type HTTPAuth struct {
	Type      string
	Secret    []byte
	Header    string // apiKey/hmac header name; ignored for bearer (always Authorization)
	Algorithm string // hmac: sha256 (default) | sha512
	Encoding  string // hmac: hex (default) | base64
	Prefix    string // hmac: required prefix before the signature, e.g. "sha256="
}

// normalize validates a and fills defaults.
func (a *HTTPAuth) normalize() error {
	if len(a.Secret) == 0 {
		return errors.New("listener: auth secret is empty")
	}
	switch a.Type {
	case flow.AuthBearer:
		a.Header = "Authorization"
	case flow.AuthAPIKey:
		if a.Header == "" {
			a.Header = DefaultAPIKeyHeader
		}
	case flow.AuthHMAC:
		if a.Header == "" {
			a.Header = DefaultHMACHeader
		}
		if a.Algorithm == "" {
			a.Algorithm = "sha256"
		}
		if a.Algorithm != "sha256" && a.Algorithm != "sha512" {
			return fmt.Errorf("listener: unsupported hmac algorithm %q (want sha256 or sha512)", a.Algorithm)
		}
		if a.Encoding == "" {
			a.Encoding = "hex"
		}
		if a.Encoding != "hex" && a.Encoding != "base64" {
			return fmt.Errorf("listener: unsupported hmac encoding %q (want hex or base64)", a.Encoding)
		}
	default:
		return fmt.Errorf("listener: unsupported auth type %q (want bearer, apiKey or hmac)", a.Type)
	}
	a.Header = http.CanonicalHeaderKey(a.Header)
	return nil
}

// secretHeader reports whether the auth header carries the secret itself (and so must never be
// copied into document properties).
func (a *HTTPAuth) secretHeader() bool { return a.Type == flow.AuthBearer || a.Type == flow.AuthAPIKey }

// checkHeader verifies bearer/apiKey credentials in constant time. Both sides are hashed first so
// the comparison does not leak the secret's length either.
func (a *HTTPAuth) checkHeader(r *http.Request) bool {
	got := r.Header.Get(a.Header)
	if a.Type == flow.AuthBearer {
		scheme, tok, ok := strings.Cut(got, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return false
		}
		got = strings.TrimSpace(tok)
	}
	if got == "" {
		return false
	}
	g, w := sha256.Sum256([]byte(got)), sha256.Sum256(a.Secret)
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// newMAC returns the HMAC for the raw request body.
func (a *HTTPAuth) newMAC() hash.Hash {
	if a.Algorithm == "sha512" {
		return hmac.New(sha512.New, a.Secret)
	}
	return hmac.New(sha256.New, a.Secret)
}

// checkSignature compares the signature header with sum (the HMAC of the body) in constant time.
func (a *HTTPAuth) checkSignature(r *http.Request, sum []byte) bool {
	v := strings.TrimSpace(r.Header.Get(a.Header))
	v, ok := strings.CutPrefix(v, a.Prefix)
	if !ok || v == "" {
		return false
	}
	var sig []byte
	var err error
	if a.Encoding == "base64" {
		sig, err = base64.StdEncoding.DecodeString(v)
	} else {
		sig, err = hex.DecodeString(v)
	}
	return err == nil && hmac.Equal(sig, sum)
}

// isLoopback reports whether addr (host:port) binds only a loopback interface. An empty host
// (":8080") binds every interface and is not loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	host, _, _ = strings.Cut(host, "%") // IPv6 zone
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
