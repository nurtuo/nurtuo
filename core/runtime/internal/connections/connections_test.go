// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"google.golang.org/protobuf/types/known/structpb"
)

type mapProv map[string]string

func (m mapProv) Get(_ context.Context, n string) (secrets.Secret, error) {
	if v, ok := m[n]; ok {
		return secrets.New(v), nil
	}
	return secrets.Secret{}, fmt.Errorf("secret %q: %w", n, secrets.ErrNotFound)
}

const good = `{"schema":"nurtuo.connection/v1","id":"erp-api","connector":"http",
"settings":{"baseUrl":"https://erp.example","auth":{"type":"bearer","token":"secret://erp-token"},"tags":["a","secret://list-secret"]}}`

func TestParse(t *testing.T) {
	d, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "erp-api" || d.Connector != "http" {
		t.Fatalf("%+v", d)
	}
	if got := d.Refs(); !slices.Equal(got, []string{"erp-token", "list-secret"}) {
		t.Fatalf("refs %v", got)
	}
	for name, doc := range map[string]string{
		"schema":    `{"schema":"x","id":"a","connector":"http"}`,
		"id":        `{"schema":"nurtuo.connection/v1","id":"A b","connector":"http"}`,
		"connector": `{"schema":"nurtuo.connection/v1","id":"a"}`,
		"unknown":   `{"schema":"nurtuo.connection/v1","id":"a","connector":"http","secrets":{"x":"y"}}`,
		"badref":    `{"schema":"nurtuo.connection/v1","id":"a","connector":"http","settings":{"p":"secret://../etc/passwd"}}`,
		"notjson":   `{`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestResolveInjectsSecretsInMemoryOnly(t *testing.T) {
	d, _ := Parse([]byte(good))
	red := secrets.NewRedactor()
	r := &Resolver{Secrets: mapProv{"erp-token": "tok-123456", "list-secret": "lst-654321"}, Redactor: red}
	res, err := r.Resolve(context.Background(), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Config.GetSettings().AsMap()
	if m["auth"].(map[string]any)["token"] != "tok-123456" || m["tags"].([]any)[1] != "lst-654321" || m["baseUrl"] != "https://erp.example" {
		t.Fatalf("settings %v", m)
	}
	// The definition itself keeps the references (it is what gets logged / described).
	if d.Settings["auth"].(map[string]any)["token"] != "secret://erp-token" {
		t.Fatal("definition was mutated with a secret value")
	}
	if red.Redact("x tok-123456 y") != "x *** y" {
		t.Fatal("resolved values not registered for redaction")
	}
	if len(res.Values) != 2 {
		t.Fatalf("values %v", res.Values)
	}
}

func TestResolveMissingSecretsReportedByNameOnly(t *testing.T) {
	d, _ := Parse([]byte(good))
	r := &Resolver{Secrets: mapProv{"list-secret": "lst-654321"}}
	_, err := r.Resolve(context.Background(), d, nil)
	if err == nil || !strings.Contains(err.Error(), `"erp-token"`) || !strings.Contains(err.Error(), `"erp-api"`) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "lst-654321") {
		t.Fatalf("value leaked: %v", err)
	}
	r = &Resolver{Secrets: mapProv{}}
	_, err = r.Resolve(context.Background(), d, nil)
	if err == nil || !strings.Contains(err.Error(), `"erp-token", "list-secret"`) {
		t.Fatalf("all missing should be listed: %v", err)
	}
	if _, err := (&Resolver{}).Resolve(context.Background(), d, nil); err == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestProviderErrorIsNotNotFound(t *testing.T) {
	d, _ := Parse([]byte(good))
	r := &Resolver{Secrets: errProv{errors.New("permissions 0644 not allowed")}}
	_, err := r.Resolve(context.Background(), d, nil)
	if err == nil || !strings.Contains(err.Error(), "permissions") || strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
}

type errProv struct{ error }

func (e errProv) Get(context.Context, string) (secrets.Secret, error) {
	return secrets.Secret{}, e.error
}

func TestLiteralSecretsRejected(t *testing.T) {
	schema, _ := structpb.NewStruct(map[string]any{"properties": map[string]any{
		"auth": map[string]any{"type": "object", "properties": map[string]any{
			"token": map[string]any{"type": "string", "x-nurtuo-secret": true},
			"type":  map[string]any{"type": "string"},
		}},
		"password": map[string]any{"x-nurtuo-secret": true},
	}})
	paths := SecretPaths(schema)
	if !paths["auth.token"] || !paths["password"] || paths["auth.type"] || len(paths) != 2 {
		t.Fatalf("paths %v", paths)
	}
	lit, _ := Parse([]byte(`{"schema":"nurtuo.connection/v1","id":"a","connector":"http","settings":{"auth":{"token":"plain-text-token","type":"bearer"}}}`))
	err := (&Resolver{Secrets: mapProv{}}).Resolveerr(lit, paths)
	if err == nil || !strings.Contains(err.Error(), "auth.token") || strings.Contains(err.Error(), "plain-text-token") {
		t.Fatalf("literal accepted or leaked: %v", err)
	}
	ok, _ := Parse([]byte(`{"schema":"nurtuo.connection/v1","id":"a","connector":"http","settings":{"auth":{"token":"secret://t"}}}`))
	if _, err := (&Resolver{Secrets: mapProv{"t": "vvv"}}).Resolve(context.Background(), ok, paths); err != nil {
		t.Fatal(err)
	}
	if SecretPaths(nil) == nil {
		t.Fatal("nil schema")
	}
}

// Resolveerr is a test helper returning only the error.
func (r *Resolver) Resolveerr(d *Definition, p map[string]bool) error {
	_, err := r.Resolve(context.Background(), d, p)
	return err
}
