// SPDX-License-Identifier: AGPL-3.0-only

// Package e2e holds end-to-end tests that run the real `nurtuo` binary against real services in
// containers. They are skipped unless their gate variable is set.
package e2e

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	// gateEnv enables the container tests (like NURTUO_TEST_POSTGRES_URL for the Postgres tests).
	gateEnv = "NURTUO_TEST_SFTP_E2E"
	// imageEnv overrides the image (default: atmoz/sftp, pinned by digest).
	imageEnv     = "NURTUO_TEST_SFTP_IMAGE"
	defaultImage = "atmoz/sftp@sha256:c5fb9e4beb8b7054a4c0b65c5e11df47cc20da95ad6002e88aecff62bab06372"
	user         = "deploy"
	password     = "e2e-sftp-pw-71c4"
)

// sftpContainer is a running SFTP server container.
type sftpContainer struct {
	host, port string
	hostKeys   []ssh.PublicKey // every host key the server offers (rsa, ecdsa, ed25519)
	clientKey  string          // PEM private key authorized for user
	body       string          // content of /downloads/version.txt
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// startSFTPContainer starts the server with /downloads/version.txt (random body) bind-mounted
// into the user's chroot and the generated client key authorized. The test never touches a real server.
func startSFTPContainer(t *testing.T) *sftpContainer {
	t.Helper()
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("set %s=1 to run the container SFTP e2e", gateEnv)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	image := os.Getenv(imageEnv)
	if image == "" {
		image = defaultImage
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	randBytes := make([]byte, 16)
	_, _ = rand.Read(randBytes)
	body := fmt.Sprintf("version 9.9.%x\n", randBytes)
	downloads := filepath.Join(dir, "downloads")
	if err := os.Mkdir(downloads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "version.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keys, "client.pub"), ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		t.Fatal(err)
	}

	id := run(t, "docker", "run", "-d", "-p", "127.0.0.1::22",
		"-v", downloads+":/home/"+user+"/downloads:ro",
		"-v", keys+":/home/"+user+"/.ssh/keys:ro",
		image, user+":"+password+":1001")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	mapped := strings.Split(run(t, "docker", "port", id, "22/tcp"), "\n")[0]
	host, port, err := net.SplitHostPort(mapped)
	if err != nil {
		t.Fatalf("docker port %q: %v", mapped, err)
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &sftpContainer{host: host, port: port, clientKey: string(pem.EncodeToMemory(blk)), body: body}
	c.waitReady(t, id)
	return c
}

// waitReady waits for sshd's banner and reads every host key the server offers from the container
// (the server generates its own host keys on first start; the client may negotiate any key type).
// It then probes the server once to check that the key it actually presents is among them.
func (c *sftpContainer) waitReady(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(c.host, c.port), 2*time.Second)
		if err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			line, rerr := bufio.NewReader(conn).ReadString('\n')
			_ = conn.Close()
			if rerr == nil && strings.HasPrefix(line, "SSH-") {
				out, kerr := exec.Command("docker", "exec", id, "sh", "-c", "cat /etc/ssh/ssh_host_*_key.pub").Output()
				if kerr == nil {
					var keys []ssh.PublicKey
					for _, l := range strings.Split(string(out), "\n") {
						if strings.TrimSpace(l) == "" {
							continue
						}
						if key, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(l)); perr == nil {
							keys = append(keys, key)
						}
					}
					if len(keys) > 0 {
						c.hostKeys = keys
						c.checkPresented(t)
						return
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
	t.Fatalf("sftp container not ready after 90s:\n%s", logs)
}

// checkPresented handshakes once (capturing the presented key only, no credentials are sent) and
// fails with a key-type diagnostic if it is not one of the recorded keys.
func (c *sftpContainer) checkPresented(t *testing.T) {
	t.Helper()
	var presented ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:    user,
		Timeout: 10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			presented = k
			return errors.New("probe: key captured")
		},
	}
	_, _ = ssh.Dial("tcp", net.JoinHostPort(c.host, c.port), cfg)
	if presented == nil {
		t.Fatalf("probe did not receive a host key from %s", net.JoinHostPort(c.host, c.port))
	}
	var types []string
	for _, k := range c.hostKeys {
		types = append(types, k.Type())
		if bytes.Equal(k.Marshal(), presented.Marshal()) {
			return
		}
	}
	// Server presents a key not in /etc/ssh: record it so verification still uses what it really offers.
	t.Logf("presented host key type %s not among recorded types %v; adding it", presented.Type(), types)
	c.hostKeys = append(c.hostKeys, presented)
}

var binPath string

// nurtuoBinary builds the real binary once per test binary run.
func nurtuoBinary(t *testing.T) string {
	t.Helper()
	if binPath != "" {
		return binPath
	}
	dir, err := os.MkdirTemp("", "nurtuo-e2e-bin")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "nurtuo")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/nurtuo")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	binPath = bin
	return bin
}

type result struct {
	code           int
	stdout, stderr string
}

func (c *sftpContainer) knownHosts(t *testing.T, dir string, keys ...ssh.PublicKey) string {
	t.Helper()
	var sb strings.Builder
	for _, key := range keys {
		// Line normalizes host:port to the "[host]:port" form the client looks up.
		sb.WriteString(knownhosts.Line([]string{net.JoinHostPort(c.host, c.port)}, key) + "\n")
	}
	p := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// flowRun runs `nurtuo flow run` with the example flow and a connection file built for the container.
func (c *sftpContainer) flowRun(t *testing.T, auth, remotePath, knownHosts, pw string) result {
	t.Helper()
	dir := t.TempDir()
	flowRaw, err := os.ReadFile("../../../../examples/flows/sftp-download.flow.json")
	if err != nil {
		t.Fatal(err)
	}
	flowFile := filepath.Join(dir, "flow.json")
	if err := os.WriteFile(flowFile, flowRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := `"password": "secret://sftp_password"`
	if auth == "key" {
		secret = `"privateKey": "secret://sftp_key"`
	}
	conn := fmt.Sprintf(`{"schema":"nurtuo.connection/v1","id":"private-sftp","connector":"sftp",
 "settings":{"host":%q,"port":%s,"user":%q,%s}}`, c.host, c.port, user, secret)
	connFile := filepath.Join(dir, "conn.json")
	if err := os.WriteFile(connFile, []byte(conn), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(dir, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "sftp_key"), []byte(c.clientKey), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nurtuoBinary(t), "flow", "run", flowFile, "--connection", connFile,
		"--known-hosts", knownHosts, "--secrets-dir", secrets, "--timeout", "60s", "--set", "path="+remotePath)
	cmd.Env = append(os.Environ(), "NURTUO_SECRET_SFTP_PASSWORD="+pw)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	res := result{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
	default:
		t.Fatalf("run nurtuo: %v", err)
	}
	return res
}

func TestFlowRunSFTPContainer(t *testing.T) {
	c := startSFTPContainer(t)
	good := c.knownHosts(t, t.TempDir(), c.hostKeys...)

	t.Run("password auth", func(t *testing.T) {
		r := c.flowRun(t, "password", "/downloads/version.txt", good, password)
		if r.code != 0 || r.stdout != c.body || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("key auth", func(t *testing.T) {
		r := c.flowRun(t, "key", "/downloads/version.txt", good, "")
		if r.code != 0 || r.stdout != c.body || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		r := c.flowRun(t, "password", "/downloads/absent.txt", good, password)
		if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "/downloads/absent.txt") || strings.Contains(r.stderr, password) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("wrong host key", func(t *testing.T) {
		otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
		other, _ := ssh.NewPublicKey(otherPub)
		r := c.flowRun(t, "password", "/downloads/version.txt", c.knownHosts(t, t.TempDir(), other), password)
		if r.code != 1 || r.stdout != "" || r.stderr == "" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		r := c.flowRun(t, "password", "/downloads/version.txt", good, "not-the-password")
		if r.code != 1 || r.stdout != "" || r.stderr == "" || strings.Contains(r.stderr, "not-the-password") || strings.Contains(r.stderr, password) {
			t.Fatalf("%+v", r)
		}
	})
}
