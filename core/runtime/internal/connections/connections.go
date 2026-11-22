// SPDX-License-Identifier: AGPL-3.0-only

// Package connections defines connection components (connector id + non-secret settings +
// secret:// references) and the Resolver that turns them into connector ConnectionConfigs by
// injecting secrets in memory.
package connections

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// SchemaID is the schema tag of a connection component document.
const SchemaID = "nurtuo.connection/v1"

// Kind is the bundle component kind of a connection.
const Kind = "connection"

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Definition is a connection component. Settings string values of the form "secret://<name>" are
// secret references; no secret value ever appears in a Definition.
type Definition struct {
	Schema    string         `json:"schema"`
	ID        string         `json:"id"`
	Connector string         `json:"connector"`
	Settings  map[string]any `json:"settings,omitempty"`
}

// Parse decodes and validates one connection document (unknown fields are rejected).
func Parse(raw []byte) (*Definition, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d Definition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("connection: %w", err)
	}
	if d.Schema != SchemaID {
		return nil, fmt.Errorf("connection: schema must be %q", SchemaID)
	}
	if !idRe.MatchString(d.ID) {
		return nil, fmt.Errorf("connection: invalid id %q (want %s)", d.ID, idRe)
	}
	if d.Connector == "" {
		return nil, fmt.Errorf("connection %q: connector is required", d.ID)
	}
	if err := checkRefs(d.ID, d.Settings, ""); err != nil {
		return nil, err
	}
	return &d, nil
}

// checkRefs validates the syntax of every secret:// reference.
func checkRefs(id string, v any, path string) error {
	var err error
	walk(v, path, func(p string, s string) any {
		if name, ok := strings.CutPrefix(s, secrets.Ref); ok && err == nil {
			if verr := secrets.ValidName(name); verr != nil {
				err = fmt.Errorf("connection %q: settings %s: %w", id, p, verr)
			}
		}
		return s
	})
	return err
}

// walk applies fn to every string leaf of v and returns the rewritten tree (maps/slices are copied).
func walk(v any, path string, fn func(path, s string) any) any {
	switch x := v.(type) {
	case string:
		return fn(path, x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			out[k] = walk(c, p, fn)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = walk(c, path+"["+strconv.Itoa(i)+"]", fn)
		}
		return out
	}
	return v
}

// Refs returns the sorted, de-duplicated secret names a definition references.
func (d *Definition) Refs() []string {
	set := map[string]bool{}
	walk(d.Settings, "", func(_, s string) any {
		if name, ok := strings.CutPrefix(s, secrets.Ref); ok {
			set[name] = true
		}
		return s
	})
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SecretPaths returns the dotted settings paths a connector's connection schema marks with
// secret: "x-nurtuo-secret": true, "writeOnly": true or format "secret" / "secret-ref" (the
// spellings the built-in connectors use). nil/empty schemas yield an empty set.
func SecretPaths(schema *structpb.Struct) map[string]bool {
	out := map[string]bool{}
	if schema != nil {
		collectSecretPaths(schema.AsMap(), "", out)
	}
	return out
}

func isSecretNode(n map[string]any) bool {
	if b, _ := n["x-nurtuo-secret"].(bool); b {
		return true
	}
	if b, _ := n["writeOnly"].(bool); b {
		return true
	}
	f, _ := n["format"].(string)
	return f == "secret" || f == "secret-ref"
}

func collectSecretPaths(node map[string]any, path string, out map[string]bool) {
	if path != "" && isSecretNode(node) {
		out[path] = true
	}
	props, _ := node["properties"].(map[string]any)
	for k, c := range props {
		if cm, ok := c.(map[string]any); ok {
			p := k
			if path != "" {
				p = path + "." + k
			}
			collectSecretPaths(cm, p, out)
		}
	}
}

// CheckLiterals fails when a setting the connector marks secret holds a literal value instead of
// a secret:// reference: secrets must never be stored in a bundle.
func (d *Definition) CheckLiterals(secretPaths map[string]bool) error {
	var bad []string
	walk(d.Settings, "", func(p string, s string) any {
		if secretPaths[p] && s != "" && !strings.HasPrefix(s, secrets.Ref) {
			bad = append(bad, p)
		}
		return s
	})
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("connection %q: settings %s must be a %s<name> reference, not a literal value (secrets are never stored in bundles)",
			d.ID, strings.Join(bad, ", "), secrets.Ref)
	}
	return nil
}

// Resolved is a connection with its secrets injected. It must stay in memory.
type Resolved struct {
	Def    *Definition
	Config *connectorv1.ConnectionConfig
	// Values are the revealed secret values (for redaction); never log them.
	Values []string
}

// Resolver injects secrets into connection definitions.
type Resolver struct {
	Secrets  secrets.Provider
	Redactor *secrets.Redactor // optional; resolved values are registered here
}

// Resolve checks the definition against secretPaths (from SecretPaths), looks up every secret and
// returns the connector config. All missing secrets are reported together, by name only.
func (r *Resolver) Resolve(ctx context.Context, d *Definition, secretPaths map[string]bool) (*Resolved, error) {
	if err := d.CheckLiterals(secretPaths); err != nil {
		return nil, err
	}
	if r.Secrets == nil {
		return nil, errors.New("no secret provider configured")
	}
	vals := map[string]secrets.Secret{}
	var missing, other []string
	for _, name := range d.Refs() {
		s, err := r.Secrets.Get(ctx, name)
		switch {
		case err == nil:
			vals[name] = s
		case errors.Is(err, secrets.ErrNotFound):
			missing = append(missing, strconv.Quote(name))
		default:
			other = append(other, err.Error())
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("connection %q: secret %s not found in the configured secret provider", d.ID, strings.Join(missing, ", "))
	}
	if len(other) > 0 {
		return nil, fmt.Errorf("connection %q: %s", d.ID, strings.Join(other, "; "))
	}
	var revealed []string
	for _, s := range vals {
		revealed = append(revealed, s.Reveal())
	}
	sort.Strings(revealed)
	if r.Redactor != nil {
		r.Redactor.Add(revealed...)
	}
	settings := walk(d.Settings, "", func(_ string, s string) any {
		if name, ok := strings.CutPrefix(s, secrets.Ref); ok {
			return vals[name].Reveal()
		}
		return s
	})
	m, _ := settings.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("connection %q: settings: %w", d.ID, err)
	}
	return &Resolved{Def: d, Config: &connectorv1.ConnectionConfig{Settings: st}, Values: revealed}, nil
}

// ResolveRef looks up a single secret://<name> reference (e.g. a listener's auth secret or TLS
// key) and registers its value with the Redactor. Errors name the secret, never the value.
func (r *Resolver) ResolveRef(ctx context.Context, ref string) (secrets.Secret, error) {
	name, ok := strings.CutPrefix(ref, secrets.Ref)
	if !ok {
		return secrets.Secret{}, fmt.Errorf("%q is not a %s<name> reference (secrets are never stored in bundles)", ref, secrets.Ref)
	}
	if err := secrets.ValidName(name); err != nil {
		return secrets.Secret{}, err
	}
	if r.Secrets == nil {
		return secrets.Secret{}, errors.New("no secret provider configured")
	}
	s, err := r.Secrets.Get(ctx, name)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return secrets.Secret{}, fmt.Errorf("secret %q not found in the configured secret provider", name)
		}
		return secrets.Secret{}, err
	}
	if r.Redactor != nil {
		r.Redactor.Add(s.Reveal())
	}
	return s, nil
}
