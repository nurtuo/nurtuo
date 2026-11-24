// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/connections"
	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	sftpTestUser     = "deploy"
	sftpTestPassword = "sftp-test-pw-9f3a"
	sftpTestBody     = "version 4.2.0\n\x00binary tail"
)

// sftpFixture is an in-process SSH/SFTP server (pattern copied from connectors/sftp/sftp_test.go,
// a _test.go helper of another module) holding /downloads/version.txt.
type sftpFixture struct {
	host, port string
	hostKey    ssh.PublicKey
	clientKey  string // PEM private key authorized on the server
}

func startSFTPFixture(t *testing.T) *sftpFixture {
	t.Helper()
	return startSFTPFixtureWith(t, nil)
}

// startSFTPFixtureWith lets the caller replace handlers of the in-memory server (e.g. to stall reads).
func startSFTPFixtureWith(t *testing.T, wrap func(*pkgsftp.Handlers), extraKeys ...ssh.Signer) *sftpFixture {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	clientPub, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSSHPub, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == sftpTestUser && string(pw) == sftpTestPassword {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == sftpTestUser && string(k.Marshal()) == string(clientSSHPub.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(hostSigner)
	for _, key := range extraKeys {
		cfg.AddHostKey(key)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	handlers := pkgsftp.InMemHandler()
	if wrap != nil {
		wrap(&handlers)
	}
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go serveSFTPConn(nc, cfg, handlers)
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	fx := &sftpFixture{host: host, port: port, hostKey: hostSigner.PublicKey(), clientKey: string(pem.EncodeToMemory(blk))}
	fx.seed(t, "/downloads/version.txt", sftpTestBody)
	return fx
}

func (fx *sftpFixture) seed(t *testing.T, p, content string) {
	t.Helper()
	c, err := ssh.Dial("tcp", net.JoinHostPort(fx.host, fx.port), &ssh.ClientConfig{
		User: sftpTestUser, Auth: []ssh.AuthMethod{ssh.Password(sftpTestPassword)},
		HostKeyCallback: ssh.FixedHostKey(fx.hostKey), HostKeyAlgorithms: []string{fx.hostKey.Type()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	fs, err := pkgsftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fs.Close() }()
	_ = fs.MkdirAll(filepath.ToSlash(filepath.Dir(p)))
	f, err := fs.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func serveSFTPConn(nc net.Conn, cfg *ssh.ServerConfig, h pkgsftp.Handlers) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range creqs {
				ok := r.Type == "subsystem" && len(r.Payload) >= 8 && string(r.Payload[4:]) == "sftp"
				_ = r.Reply(ok, nil)
				if ok {
					go func() {
						_ = pkgsftp.NewRequestServer(ch, h).Serve()
						_ = ch.Close()
					}()
				}
			}
		}()
	}
}

// knownHostsFile writes a known_hosts file trusting key for the fixture's host:port.
func (fx *sftpFixture) knownHostsFile(t *testing.T, dir string, key ssh.PublicKey) string {
	t.Helper()
	line := knownhosts.Line([]string{net.JoinHostPort(fx.host, fx.port)}, key)
	return writeTemp(t, dir, "known_hosts", line+"\n")
}

// connection returns the connection file body; auth is "password" or "key".
func (fx *sftpFixture) connection(auth string, extra ...string) string {
	port, _ := strconv.Atoi(fx.port)
	kv := []string{"host", `"` + fx.host + `"`, "port", strconv.Itoa(port), "user", `"` + sftpTestUser + `"`}
	if auth == "key" {
		kv = append(kv, "privateKey", `"secret://sftp_key"`)
	} else {
		kv = append(kv, "password", `"secret://sftp_password"`)
	}
	kv = append(kv, extra...)
	return connJSON("private-sftp", "sftp", stubSettings(kv...))
}

// sftpEnv holds the files a "nurtuo flow run" SFTP invocation needs.
type sftpEnv struct {
	flow, conn, knownHosts string
	secretsDir             string
}

func newSFTPEnv(t *testing.T, fx *sftpFixture, auth string, extra ...string) *sftpEnv {
	t.Helper()
	dir := t.TempDir()
	sd := filepath.Join(dir, "secrets")
	if err := os.Mkdir(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTemp(t, sd, "sftp_key", fx.clientKey)
	writeTemp(t, sd, "sftp_password", sftpTestPassword)
	return &sftpEnv{
		flow:       writeTemp(t, dir, "flow.json", flowJSON(manualStart, "sftp", "private-sftp")),
		conn:       writeTemp(t, dir, "conn.json", fx.connection(auth, extra...)),
		knownHosts: fx.knownHostsFile(t, dir, fx.hostKey),
		secretsDir: sd,
	}
}

func (e *sftpEnv) args(extra ...string) []string {
	return append([]string{e.flow, "--connection", e.conn, "--known-hosts", e.knownHosts, "--secrets-dir", e.secretsDir}, extra...)
}

func TestFlowRunSFTPInProc(t *testing.T) {
	fx := startSFTPFixture(t)
	t.Setenv("NURTUO_SECRET_SFTP_PASSWORD", sftpTestPassword) // env source
	env := newSFTPEnv(t, fx, "password")
	env.secretsDir = ""
	t.Run("GET returns the file bytes", func(t *testing.T) {
		code, out, errs := runFlow(t, nil, env.args("--set", "path=/downloads/version.txt")...)
		if code != 0 || out != sftpTestBody || errs != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		code, out, errs := runFlow(t, nil, env.args("--set", "path=/downloads/absent.txt")...)
		if code != 1 || out != "" || !strings.Contains(errs, "/downloads/absent.txt") || strings.Contains(errs, sftpTestPassword) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("missing path property", func(t *testing.T) {
		code, out, _ := runFlow(t, nil, env.args()...)
		if code != 1 || out != "" {
			t.Fatalf("code=%d stdout=%q", code, out)
		}
	})
	t.Run("default known_hosts is read from the home directory", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		kh, _ := os.ReadFile(env.knownHosts)
		writeTemp(t, filepath.Join(home, ".ssh"), "known_hosts", string(kh))
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		args := []string{env.flow, "--connection", env.conn, "--set", "path=/downloads/version.txt"}
		code, out, errs := runFlow(t, nil, args...)
		if code != 0 || out != sftpTestBody || errs != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("missing default known_hosts", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		code, out, errs := runFlow(t, nil, env.flow, "--connection", env.conn, "--set", "path=/downloads/version.txt")
		if code != 1 || out != "" || !strings.Contains(errs, "--known-hosts") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
}

func TestFlowRunSFTPKeyAuth(t *testing.T) {
	fx := startSFTPFixture(t)
	t.Setenv("NURTUO_SECRET_SFTP_PASSWORD", "") // env wins over --secrets-dir
	t.Setenv("NURTUO_SECRET_SFTP_KEY", "")
	env := newSFTPEnv(t, fx, "key")
	code, out, errs := runFlow(t, nil, env.args("--set", "path=/downloads/version.txt")...)
	if code != 0 || out != sftpTestBody || errs != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	// The same key through the environment (single line is not needed: env keeps newlines).
	if err := os.Remove(filepath.Join(env.secretsDir, "sftp_key")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NURTUO_SECRET_SFTP_KEY", fx.clientKey)
	code, out, errs = runFlow(t, nil, env.args("--set", "path=/downloads/version.txt")...)
	if code != 0 || out != sftpTestBody || errs != "" {
		t.Fatalf("env: code=%d stdout=%q stderr=%q", code, out, errs)
	}
}

func TestFlowRunSFTPWrongPassword(t *testing.T) {
	fx := startSFTPFixture(t)
	t.Setenv("NURTUO_SECRET_SFTP_PASSWORD", "") // env wins over --secrets-dir
	t.Setenv("NURTUO_SECRET_SFTP_KEY", "")
	env := newSFTPEnv(t, fx, "password")
	writeTemp(t, env.secretsDir, "sftp_password", "definitely-wrong-pw")
	code, out, errs := runFlow(t, nil, env.args("--set", "path=/downloads/version.txt")...)
	if code != 1 || out != "" || errs == "" || strings.Contains(errs, "definitely-wrong-pw") || strings.Contains(errs, sftpTestPassword) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
}

func TestFlowRunSFTPHostKeyMismatch(t *testing.T) {
	fx := startSFTPFixture(t)
	env := newSFTPEnv(t, fx, "password")
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewPublicKey(otherPub)
	env.knownHosts = fx.knownHostsFile(t, t.TempDir(), other)
	code, out, errs := runFlow(t, nil, env.args("--set", "path=/downloads/version.txt")...)
	if code != 1 || out != "" || errs == "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	// An empty known_hosts (unknown host) fails as well.
	env.knownHosts = writeTemp(t, t.TempDir(), "known_hosts", "")
	code, out, _ = runFlow(t, nil, env.args("--set", "path=/downloads/version.txt")...)
	if code != 1 || out != "" {
		t.Fatalf("unknown host: code=%d stdout=%q", code, out)
	}
}

func TestFlowRunInsecureHostKeyWarning(t *testing.T) {
	fx := startSFTPFixture(t)
	env := newSFTPEnv(t, fx, "password")
	before, _ := os.ReadFile(env.conn)
	args := []string{env.flow, "--connection", env.conn, "--secrets-dir", env.secretsDir, "--insecure-host-key", "--set", "path=/downloads/version.txt"}
	// No known_hosts is needed with --insecure-host-key; HOME has none.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	code, out, errs := runFlow(t, nil, args...)
	if code != 0 || out != sftpTestBody || !strings.Contains(errs, "warning: host key verification is disabled") || strings.Contains(errs, sftpTestPassword) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	if after, _ := os.ReadFile(env.conn); string(after) != string(before) {
		t.Fatalf("the connection file was modified:\n%s", after)
	}
	t.Run("warning is JSON with --json", func(t *testing.T) {
		code, _, errs := runFlow(t, nil, append(args, "--json")...)
		first, _, _ := strings.Cut(strings.TrimSpace(errs), "\n")
		if code != 0 || !json.Valid([]byte(first)) || !strings.Contains(first, "insecure_host_key") {
			t.Fatalf("code=%d stderr=%q", code, errs)
		}
	})
	t.Run("insecure policy inside the connection file without the flag is exit 2", func(t *testing.T) {
		env := newSFTPEnv(t, fx, "password", "hostKeyPolicy", `"insecure"`)
		code, out, errs := runFlow(t, nil, env.flow, "--connection", env.conn, "--secrets-dir", env.secretsDir, "--set", "path=/downloads/version.txt")
		if code != 2 || out != "" || !strings.Contains(errs, "--insecure-host-key") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
	t.Run("the flag overrides an insecure policy in the file", func(t *testing.T) {
		env := newSFTPEnv(t, fx, "password", "hostKeyPolicy", `"insecure"`)
		code, out, errs := runFlow(t, nil, env.flow, "--connection", env.conn, "--secrets-dir", env.secretsDir, "--insecure-host-key", "--set", "path=/downloads/version.txt")
		if code != 0 || out != sftpTestBody || !strings.Contains(errs, "warning: host key verification is disabled") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
		}
	})
}

// stallReaderAt blocks every read until release is closed.
type stallReaderAt struct{ release chan struct{} }

func (r stallReaderAt) ReadAt([]byte, int64) (int, error) {
	<-r.release
	return 0, io.EOF
}

type stallFileGet struct {
	release chan struct{}
	inner   pkgsftp.FileReader
}

func (g stallFileGet) Fileread(req *pkgsftp.Request) (io.ReaderAt, error) {
	return stallReaderAt{g.release}, nil
}

// A stalled GET must honour cancellation after the handshake: exit 1, empty stdout.
func TestFlowRunSFTPCancelStalledGet(t *testing.T) {
	release := make(chan struct{})
	fx := startSFTPFixtureWith(t, func(h *pkgsftp.Handlers) { h.FileGet = stallFileGet{release: release, inner: h.FileGet} })
	t.Cleanup(func() { close(release) })
	t.Setenv("NURTUO_SECRET_SFTP_PASSWORD", "")
	env := newSFTPEnv(t, fx, "password")
	type result struct {
		code      int
		out, errs string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errs := runFlow(t, nil, env.args("--set", "path=/downloads/version.txt", "--timeout", "1s")...)
		done <- result{code, out, errs}
	}()
	select {
	case r := <-done:
		if r.code != 1 || r.out != "" || r.errs == "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", r.code, r.out, r.errs)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stalled GET ignored cancellation")
	}
}

func TestFlowRunExamples(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "examples")
	flowFile := filepath.Join(root, "flows", "sftp-download.flow.json")
	connFile := filepath.Join(root, "connections", "sftp.connection.json")
	f, err := loadFlow(flowFile)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := loadConnections([]string{connFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crossCheck(f, defs, nativeConnectors()); err != nil {
		t.Fatal(err)
	}
}

func TestFlowRunHostKeyPrecedence(t *testing.T) {
	const flagKH, inlineKH, homeKH = "flag-known-hosts\n", "inline-known-hosts\n", "home-known-hosts\n"
	tests := []struct {
		name     string
		settings map[string]any
		useFlag  bool
		insecure bool
		wantKH   string // expected settings.knownHosts after applying
		wantExit int    // 0 = no error
	}{
		{name: "flag beats inline", settings: map[string]any{"knownHosts": inlineKH}, useFlag: true, wantKH: flagKH},
		{name: "flag beats inline with explicit knownHosts policy", settings: map[string]any{"knownHosts": inlineKH, "hostKeyPolicy": sftpPolicyKnownHosts}, useFlag: true, wantKH: flagKH},
		{name: "inline kept when no flag", settings: map[string]any{"knownHosts": inlineKH}, wantKH: inlineKH},
		{name: "home known_hosts when neither", settings: map[string]any{}, wantKH: homeKH},
		{name: "fingerprint ignores flag", settings: map[string]any{"hostKeyPolicy": "fingerprint", "hostKeyFingerprint": "SHA256:abc"}, useFlag: true, wantKH: ""},
		{name: "file-level insecure without flag is exit 2", settings: map[string]any{"hostKeyPolicy": "insecure"}, useFlag: true, wantExit: exitUsage},
		{name: "flag overrides file-level insecure", settings: map[string]any{"hostKeyPolicy": "insecure"}, insecure: true, wantKH: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTemp(t, filepath.Join(home, ".ssh"), "known_hosts", homeKH)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			o := &flowRunOptions{insecure: tc.insecure}
			if tc.useFlag {
				o.knownHosts = writeTemp(t, t.TempDir(), "kh", flagKH)
			}
			def := &connections.Definition{ID: "c", Connector: "sftp", Settings: tc.settings}
			err := applyHostKeys([]*connections.Definition{def}, []string{"c"}, o, io.Discard)
			if tc.wantExit != 0 {
				var fe *flowRunError
				if !errors.As(err, &fe) || fe.exit != tc.wantExit {
					t.Fatalf("err = %v, want exit %d", err, tc.wantExit)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := def.Settings["knownHosts"].(string); got != tc.wantKH {
				t.Fatalf("knownHosts = %q, want %q", got, tc.wantKH)
			}
		})
	}

	t.Run("file-level insecure without flag makes no connection", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		accepted := make(chan struct{}, 1)
		go func() {
			if c, err := l.Accept(); err == nil {
				accepted <- struct{}{}
				_ = c.Close()
			}
		}()
		host, port, _ := net.SplitHostPort(l.Addr().String())
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		hk, _ := ssh.NewPublicKey(pub)
		fx := &sftpFixture{host: host, port: port, hostKey: hk}
		env := newSFTPEnv(t, fx, "password", "hostKeyPolicy", `"insecure"`)
		code, out, _ := runFlow(t, nil, env.args()...)
		if code != 2 || out != "" {
			t.Fatalf("code=%d stdout=%q", code, out)
		}
		select {
		case <-accepted:
			t.Fatal("a connection was attempted")
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("empty known_hosts content is exit 1", func(t *testing.T) {
		fx := startSFTPFixture(t)
		env := newSFTPEnv(t, fx, "password")
		env.knownHosts = writeTemp(t, t.TempDir(), "known_hosts", "")
		code, out, _ := runFlow(t, nil, env.args("--set", "path=/downloads/version.txt")...)
		if code != 1 || out != "" {
			t.Fatalf("code=%d stdout=%q", code, out)
		}
	})
}

func TestFlowHostKeyScan(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	fx := startSFTPFixtureWith(t, nil, signer)
	var expected scannedHostKey
	client, err := ssh.Dial("tcp", net.JoinHostPort(fx.host, fx.port), &ssh.ClientConfig{
		User: sftpTestUser, Auth: []ssh.AuthMethod{ssh.Password(sftpTestPassword)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			expected = scannedHostKey{Type: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	var out, stderr bytes.Buffer
	if code := flowMain([]string{"host-key", fx.host, "--port", fx.port, "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	var result struct{ Keys []scannedHostKey }
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Keys) != 1 || result.Keys[0] != expected {
		t.Fatalf("scanned %+v, want %+v", result.Keys, expected)
	}
}

func TestFlowRunSFTPOutputLimit(t *testing.T) {
	fx := startSFTPFixture(t)
	fx.seed(t, "/downloads/large", strings.Repeat("x", 2<<20))
	env := newSFTPEnv(t, fx, "password")
	code, out, stderr := runFlow(t, nil, env.args("--json", "--set", "path=/downloads/large", "--max-output-bytes", "65536", "--max-documents", "20")...)
	if code != exitExec || out != "" || !strings.Contains(stderr, "output_too_large") {
		t.Fatalf("expected atomic output overflow; exit %d, stdout bytes %d, stderr %s", code, len(out), stderr)
	}
}

func TestFlowHostKeyScanSendsNoAuth(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	auth := make(chan string, 1)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, _ []byte) (*ssh.Permissions, error) {
			return nil, errors.New("denied")
		},
		AuthLogCallback: func(_ ssh.ConnMetadata, method string, _ error) {
			select {
			case auth <- method:
			default:
			}
		}}
	cfg.AddHostKey(signer)
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			_, _, _, err = ssh.NewServerConn(conn, cfg)
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if key, err := scanHostKey(ctx, l.Addr().String()); err != nil || key.Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatalf("scan got %+v, error %v", key, err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SSH handshake unexpectedly completed")
		}
	case <-ctx.Done():
		t.Fatal("scan did not close the SSH socket")
	}
	select {
	case method := <-auth:
		t.Fatalf("scan attempted %s authentication", method)
	default:
	}
}

func TestFlowHostKeyScanClosesSilentPeerOnDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			if err = conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err == nil {
				_, err = io.Copy(io.Discard, conn)
			}
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := scanHostKey(ctx, l.Addr().String()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scan error = %v, want deadline exceeded", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting for EOF: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent peer socket stayed open")
	}
}
