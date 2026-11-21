// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigPath(t *testing.T) {
	env := func(k string) string {
		if k == "ProgramData" {
			return `D:\PD`
		}
		return ""
	}
	cases := map[string]string{
		"windows": `D:\PD\Nurtuo\runtime.yaml`,
		"linux":   "/etc/nurtuo/runtime.yaml",
		"darwin":  "/Library/Application Support/Nurtuo/runtime.yaml",
	}
	for goos, want := range cases {
		if got := defaultConfigPath(goos, env); got != want {
			t.Errorf("%s: got %q want %q", goos, got, want)
		}
	}
	if got := defaultConfigPath("windows", func(string) string { return "" }); got != `C:\ProgramData\Nurtuo\runtime.yaml` {
		t.Errorf("windows fallback: %q", got)
	}
}

func TestParseServiceArgs(t *testing.T) {
	sa, err := parseServiceArgs([]string{"install", "--config", "x.yaml"}, "def.yaml")
	if err != nil || sa.Action != "install" || sa.ConfigPath != "x.yaml" {
		t.Fatalf("got %+v %v", sa, err)
	}
	sa, err = parseServiceArgs([]string{"status"}, "def.yaml")
	if err != nil || sa.ConfigPath != "def.yaml" {
		t.Fatalf("got %+v %v", sa, err)
	}
	for _, bad := range [][]string{nil, {"bogus"}, {"start", "extra"}, {"run", "--nope"}} {
		if _, err := parseServiceArgs(bad, "d"); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestServiceConfigArguments(t *testing.T) {
	c := serviceConfig("/etc/nurtuo/runtime.yaml")
	got := strings.Join(c.Arguments, " ")
	if c.Name != "nurtuo" || got != "service run --config /etc/nurtuo/runtime.yaml" {
		t.Fatalf("%q %q", c.Name, got)
	}
}

func TestDispatchServiceErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := dispatch([]string{"service"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "missing action") {
		t.Fatalf("code=%d %q", code, errb.String())
	}
	errb.Reset()
	if code := dispatch([]string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if code := dispatch([]string{"role"}, &out, &errb); code != 1 {
		t.Fatalf("stub code=%d", code)
	}
}

// The program is the service.Interface; the OS manager only calls Start (non-blocking) and Stop.
func TestProgramStartStop(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	p := &program{run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil
	}}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("run not started")
	}
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before run finished")
	}
}

func TestProgramStopWithoutStartAndRunError(t *testing.T) {
	if err := (&program{}).Stop(nil); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	p := &program{run: func(context.Context) error { return errors.New("boom") }, log: func(e error) { got <- e }}
	_ = p.Start(nil)
	select {
	case e := <-got:
		if e.Error() != "boom" {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("error not reported")
	}
	_ = p.Stop(nil)
}
