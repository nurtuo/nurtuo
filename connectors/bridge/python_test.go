// SPDX-License-Identifier: Apache-2.0

package bridge_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/connectors/bridge"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// pythonBridgeSrc is the source root of the Python bridge package, relative to this package.
const pythonBridgeSrc = "../../bridges/python/src"

// findPython returns an interpreter that can import the bridge's dependencies, or skips.
// Set NURTUO_TEST_PYTHON to point at a virtualenv interpreter (pip install -e bridges/python).
func findPython(t *testing.T) string {
	t.Helper()
	candidates := []string{"python3", "python", "py"}
	if p := os.Getenv("NURTUO_TEST_PYTHON"); p != "" {
		candidates = []string{p}
	}
	src, err := filepath.Abs(pythonBridgeSrc)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, "-c", "import nurtuo_bridge.examples.echo")
		cmd.Env = append(os.Environ(), "PYTHONPATH="+src)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return path
		}
		last = strings.TrimSpace(string(out))
	}
	if last != "" {
		t.Skipf("no Python with the bridge dependencies (grpcio>=1.85.3, grpcio-health-checking, protobuf); last error: %s",
			last[max(0, len(last)-200):])
	}
	t.Skip("python not found (set NURTUO_TEST_PYTHON)")
	return ""
}

func pythonConfig(t *testing.T, python, spec string) bridge.Config {
	t.Helper()
	src, _ := filepath.Abs(pythonBridgeSrc)
	args := []string{"-u", "-m", "nurtuo_bridge", spec}
	if filepath.Base(python) == "py" || filepath.Base(python) == "py.exe" {
		args = append([]string{"-3"}, args...)
	}
	return bridge.Config{
		Command:      python,
		Args:         args,
		Env:          []string{"PYTHONPATH=" + src},
		StartTimeout: 30 * time.Second,
		Stderr:       testWriter{t},
	}
}

// TestPythonEchoBridgeConforms runs the Python example bridge through the shared contract
// suite via Options.Dial: this is the end-to-end check of launcher, handshake, token auth,
// streaming and per-document status semantics against a real Python process.
func TestPythonEchoBridgeConforms(t *testing.T) {
	python := findPython(t)
	b, err := bridge.Start(t.Context(), pythonConfig(t, python, "nurtuo_bridge.examples.echo:EchoConnector"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if !strings.HasPrefix(b.Implementation(), "python/") {
		t.Errorf("Implementation() = %q", b.Implementation())
	}

	settings, err := structpb.NewStruct(map[string]any{"api_token": "tok-SECRET-123"})
	if err != nil {
		t.Fatal(err)
	}
	echo := &connectorv1.ExecuteStart{
		OperationType: connectorv1.OperationType_OPERATION_TYPE_EXECUTE,
		CustomType:    "UPPERCASE",
	}
	contracttest.Run(t, contracttest.Options{
		Dial: func(t testing.TB) *grpc.ClientConn {
			cc, err := b.NewConn()
			if err != nil {
				t.Fatal(err)
			}
			return cc
		},
		Connection: &connectorv1.ConnectionConfig{Settings: settings},
		Secrets:    []string{"tok-SECRET-123"},
		Timeout:    30 * time.Second,
		Execute: []contracttest.ExecuteCase{{
			Name:  "statuses and chunking",
			Start: echo,
			Documents: []contracttest.Document{
				{ID: "ok", Data: []byte("hello")},
				{ID: "rejected", Data: []byte("!reject this")},
				{ID: "crashes", Data: []byte("!crash")},
				{ID: "leaks", Data: []byte("!leak")},
				{ID: "big", Data: bytes.Repeat([]byte("abc"), 400_000)}, // 1.2 MB in 100 kB chunks
				{ID: "empty"},
			},
			ChunkSize: 100_000,
			Want: map[string]connectorv1.DocumentStatus{
				"ok":       connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
				"rejected": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
				"crashes":  connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
				"leaks":    connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
				"big":      connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
				"empty":    connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
			},
		}},
	})
}

// TestPythonBridgeStopsWhenClosed checks the shutdown path on a real Python child: closing
// stdin makes it exit by itself, well before the kill timeout.
func TestPythonBridgeStopsWhenClosed(t *testing.T) {
	python := findPython(t)
	cfg := pythonConfig(t, python, "nurtuo_bridge.examples.echo:EchoConnector")
	cfg.StopTimeout = 20 * time.Second
	b, err := bridge.Start(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	begin := time.Now()
	_ = b.Close()
	if d := time.Since(begin); d > 15*time.Second {
		t.Fatalf("Close took %s; the bridge did not exit on stdin EOF", d)
	}
}
