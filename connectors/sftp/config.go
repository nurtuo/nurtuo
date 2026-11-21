// SPDX-License-Identifier: Apache-2.0

package sftp

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"google.golang.org/protobuf/types/known/structpb"
)

// Host key policies.
const (
	PolicyKnownHosts  = "known_hosts"
	PolicyFingerprint = "fingerprint"
	PolicyInsecure    = "insecure"
)

type config struct {
	Host, User, Password, PrivateKey, Passphrase string
	Port                                         int
	Policy, KnownHosts, Fingerprint, Root        string
	Timeout                                      time.Duration
	MaxListEntries                               int
}

// DefaultMaxListEntries caps QUERY listings unless maxListEntries is configured.
const DefaultMaxListEntries = 10000

func str(s *structpb.Struct, k string) string {
	if v, ok := s.GetFields()[k]; ok {
		return v.GetStringValue()
	}
	return ""
}

func num(s *structpb.Struct, k string, def float64) float64 {
	if v, ok := s.GetFields()[k]; ok {
		if _, isNum := v.GetKind().(*structpb.Value_NumberValue); isNum {
			return v.GetNumberValue()
		}
	}
	return def
}

func parseConfig(s *structpb.Struct) (*config, error) {
	c := &config{
		Host: str(s, "host"), User: str(s, "user"), Password: str(s, "password"),
		PrivateKey: str(s, "privateKey"), Passphrase: str(s, "passphrase"),
		Port: int(num(s, "port", 22)), Policy: str(s, "hostKeyPolicy"),
		KnownHosts: str(s, "knownHosts"), Fingerprint: str(s, "hostKeyFingerprint"),
		Root: str(s, "rootPath"), Timeout: time.Duration(num(s, "timeoutMs", 30000)) * time.Millisecond,
		MaxListEntries: int(num(s, "maxListEntries", DefaultMaxListEntries)),
	}
	if c.Policy == "" {
		c.Policy = PolicyKnownHosts
	}
	if c.Root == "" {
		c.Root = "/"
	}
	switch {
	case c.Host == "":
		return nil, errors.New("connection: host is required")
	case c.User == "":
		return nil, errors.New("connection: user is required")
	case c.Password == "" && c.PrivateKey == "":
		return nil, errors.New("connection: password or privateKey is required")
	case c.MaxListEntries < 1:
		return nil, errors.New("connection: maxListEntries must be at least 1")
	case c.Port < 1 || c.Port > 65535:
		return nil, errors.New("connection: port out of range")
	}
	switch c.Policy {
	case PolicyKnownHosts:
		if c.KnownHosts == "" {
			return nil, errors.New("connection: hostKeyPolicy known_hosts requires knownHosts")
		}
	case PolicyFingerprint:
		if c.Fingerprint == "" {
			return nil, errors.New("connection: hostKeyPolicy fingerprint requires hostKeyFingerprint")
		}
	case PolicyInsecure:
	default:
		return nil, fmt.Errorf("connection: unknown hostKeyPolicy %q", c.Policy)
	}
	return c, nil
}

func (c *config) secrets() []string {
	return []string{c.Password, c.PrivateKey, c.Passphrase}
}

func (c *config) addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

func (c *config) sshConfig() (*ssh.ClientConfig, error) {
	var auth []ssh.AuthMethod
	if c.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if c.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(c.PrivateKey), []byte(c.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(c.PrivateKey))
		}
		if err != nil {
			return nil, errors.New("connection: cannot parse privateKey")
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if c.Password != "" {
		auth = append(auth, ssh.Password(c.Password))
	}
	var cb ssh.HostKeyCallback
	switch c.Policy {
	case PolicyKnownHosts:
		// x/crypto only parses known_hosts from files, so stage the content briefly.
		f, err := os.CreateTemp("", "nurtuo-known-hosts-*")
		if err != nil {
			return nil, errors.New("connection: cannot stage knownHosts")
		}
		defer func() { _ = os.Remove(f.Name()) }()
		_, werr := f.WriteString(c.KnownHosts)
		_ = f.Close()
		if werr != nil {
			return nil, errors.New("connection: cannot stage knownHosts")
		}
		kh, err := knownhosts.New(f.Name())
		if err != nil {
			return nil, errors.New("connection: invalid knownHosts")
		}
		cb = kh
	case PolicyFingerprint:
		want := c.Fingerprint
		cb = func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(k); got != want {
				return fmt.Errorf("host key mismatch: got %s, want %s", got, want)
			}
			return nil
		}
	default:
		cb = ssh.InsecureIgnoreHostKey() //nolint:gosec // explicit opt-in policy
	}
	return &ssh.ClientConfig{User: c.User, Auth: auth, HostKeyCallback: cb, Timeout: c.Timeout}, nil
}

// remote confines a client-supplied path below the configured root.
func (c *config) remote(p string) string {
	return path.Join(c.Root, path.Clean("/"+strings.ReplaceAll(p, "\\", "/")))
}
