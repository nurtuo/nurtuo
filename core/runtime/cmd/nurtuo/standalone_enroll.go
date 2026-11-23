// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// standaloneAdminEmail is the bootstrap owner of a standalone install; its password is the
// generated admin token (see controlPlaneEnv).
const standaloneAdminEmail = "admin@nurtuo.local"

// standaloneEnvironment is the environment the standalone runtime is registered in.
const standaloneEnvironment = "standalone"

// sessionCookie is the control plane session cookie set by POST /v1/auth/login.
const sessionCookie = "__Host-nurtuo_session"

// registerStandaloneRuntime makes sure the control plane knows the runtime standaloneRuntimeID,
// which the gateway requires before it accepts the runtime's session. It logs in as the
// bootstrap owner, registers the runtime only when GET /v1/runtimes/<id> says it is missing (so a
// restart neither fails nor overwrites edits made since) and revokes the session again.
func registerStandaloneRuntime(ctx context.Context, hc *http.Client, baseURL, email, password string) (err error) {
	base := strings.TrimRight(baseURL, "/")
	jwt, err := standaloneLogin(ctx, hc, base, email, password)
	if err != nil {
		return err
	}
	defer func() {
		// Best effort: the session expires on its own if logout fails.
		_, _, _ = apiCall(ctx, hc, http.MethodPost, base+"/v1/auth/logout", jwt, nil)
	}()

	code, body, err := apiCall(ctx, hc, http.MethodGet, base+"/v1/runtimes/"+url.PathEscape(standaloneRuntimeID), jwt, nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
	default:
		return fmt.Errorf("get runtime %s: HTTP %d: %s", standaloneRuntimeID, code, body)
	}
	code, body, err = apiCall(ctx, hc, http.MethodPost, base+"/v1/runtimes", jwt, map[string]any{
		"id": standaloneRuntimeID, "name": standaloneRuntimeID, "environment": standaloneEnvironment,
	})
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return fmt.Errorf("register runtime %s: HTTP %d: %s", standaloneRuntimeID, code, body)
	}
	return nil
}

// standaloneLogin returns the session JWT, used as a bearer token (no CSRF header needed).
func standaloneLogin(ctx context.Context, hc *http.Client, base, email, password string) (string, error) {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/auth/login", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login as %s: HTTP %d", email, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c.Value, nil
		}
	}
	return "", errors.New("login: no session cookie in response")
}

func apiCall(ctx context.Context, hc *http.Client, method, u, bearer string, body any) (int, string, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(raw), nil
}

// registerRealRuntime is the Stack.Register step of `nurtuo standalone`.
func registerRealRuntime(ctx context.Context, env RuntimeEnv) error {
	hc := &http.Client{Timeout: 15 * time.Second}
	return registerStandaloneRuntime(ctx, hc, env.ControlPlaneURL, standaloneAdminEmail, env.Creds.AdminToken)
}
