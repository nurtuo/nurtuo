// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	httpconnector "github.com/nurtuo/nurtuo/connectors/http"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestPinnedDialUsesOnlyExactVettedDestination(t *testing.T) {
	pins := map[string]string{
		"employee.test:443": "192.0.2.10",
		"v6.test:5432":      "2001:db8::21",
		"invalid.test:80":   "unexpected.example",
	}
	for logical, want := range map[string]string{
		"employee.test:443": "192.0.2.10:443",
		"v6.test:5432":      "[2001:db8::21]:5432",
	} {
		got, err := pinnedAddress(pins, logical)
		if err != nil || got != want {
			t.Errorf("pinnedAddress(%q) = %q, %v; want %q", logical, got, err, want)
		}
	}
	for _, logical := range []string{"employee.test:80", "unknown.test:443", "invalid.test:80"} {
		if _, err := pinnedAddress(pins, logical); !errors.Is(err, errUnpinnedDial) {
			t.Errorf("pinnedAddress(%q) returned %v; want unavailable error", logical, err)
		}
	}
}

func TestPinnedHTTPTransportRefusesMethodsOtherThanGET(t *testing.T) {
	// A nil transport is enough because rejection must occur before reaching any socket.
	req := httptest.NewRequest(http.MethodPost, "https://employee.test/", nil)
	if response, err := (getOnlyTransport{}).RoundTrip(req); err == nil || response != nil {
		t.Fatal("the test run transport accepted a POST")
	}
	req = httptest.NewRequest(http.MethodGet, "https://employee.test/", nil)
	req.Header.Set("X-HTTP-Method-Override", http.MethodPost)
	if response, err := (getOnlyTransport{}).RoundTrip(req); err == nil || response != nil {
		t.Fatal("the test run transport accepted a method override")
	}
}

func TestPinnedHTTPDialsVettedIPAndKeepsHost(t *testing.T) {
	var arrived atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if _, err := w.Write([]byte(r.Host)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	host := net.JoinHostPort("employee.test", port)
	plan, err := json.Marshal(dialPlan{HTTP: map[string]string{host: "127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NURTUO_TEST_RUN_DIAL_PLAN", string(plan))
	registry, err := pinnedConnectors()
	if err != nil {
		t.Fatal(err)
	}
	connector, err := registry.New("http")
	if err != nil {
		t.Fatal(err)
	}
	client := connector.(*httpconnector.Connector).Client
	defer client.CloseIdleConnections()
	base := url.URL{Scheme: "http", Host: host}
	response, err := client.Get(base.String())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != host {
		t.Fatalf("response body = %q, %v; want logical host %q", body, err, host)
	}
	base.Path = "/redirect"
	if response, err := client.Get(base.String()); err == nil || response == nil || response.StatusCode != http.StatusFound {
		t.Fatalf("redirect was followed: %v", err)
	}
	if got := arrived.Load(); got != 2 {
		t.Errorf("got %d requests; want one GET and one redirect response", got)
	}
}

func TestPinnedPostgresTestDoesNotConnectToUnvettedPeer(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	plan, err := json.Marshal(dialPlan{Postgres: map[string]string{
		net.JoinHostPort("vetted.test", strconv.Itoa(port)): "192.0.2.10",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NURTUO_TEST_RUN_DIAL_PLAN", string(plan))
	registry, err := pinnedConnectors()
	if err != nil {
		t.Fatal(err)
	}
	c, err := registry.New("postgres")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := structpb.NewStruct(map[string]any{
		"host": "127.0.0.1", "port": port, "database": "db", "user": "user",
		"password": "fixture-password", "sslMode": "disable", "connectTimeoutSeconds": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Test(t.Context(), &connectorv1.TestRequest{
		Connection: &connectorv1.ConnectionConfig{Settings: settings},
	})
	if err != nil || result.GetOk() || result.GetDiagnostic() != errUnpinnedDial.Error() {
		t.Errorf("expected dial policy refusal, got %v, %v", result, err)
	}
	if err := listener.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.AcceptTCP()
	if err == nil {
		_ = conn.Close()
		t.Fatal("unvetted peer received a connection")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatal(err)
	}
}
