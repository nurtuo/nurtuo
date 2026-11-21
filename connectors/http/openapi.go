// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"sort"
	"strings"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} } //nolint:gosec // opt-in per connection

// OperationInfo is one operation parsed from an OpenAPI 3 document.
type OperationInfo struct {
	ID          string // operationId, or "<METHOD> <path>" when absent
	Method      string
	Path        string
	Summary     string
	RequestBody json.RawMessage // JSON Schema of the application/json request body, if any
	Response    json.RawMessage // JSON Schema of the first 2xx application/json response, if any
}

// ParseOpenAPI parses an OpenAPI 3.x document in JSON form into operation descriptors.
// Local "#/components/..." $refs in schemas are inlined one level deep; schemas keep other refs.
func ParseOpenAPI(doc []byte) ([]OperationInfo, error) {
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("openapi: only JSON documents are supported: %w", err)
	}
	if v, _ := root["openapi"].(string); !strings.HasPrefix(v, "3.") {
		return nil, errors.New("openapi: not an OpenAPI 3.x document")
	}
	paths, _ := root["paths"].(map[string]any)
	var out []OperationInfo
	for p, item := range paths {
		im, _ := item.(map[string]any)
		for _, m := range methods {
			op, ok := im[strings.ToLower(m)].(map[string]any)
			if !ok {
				continue
			}
			info := OperationInfo{ID: str(op["operationId"]), Method: m, Path: p, Summary: str(op["summary"])}
			if info.ID == "" {
				info.ID = m + " " + p
			}
			if rb, ok := op["requestBody"].(map[string]any); ok {
				info.RequestBody = jsonSchema(root, resolve(root, rb))
			}
			if rs, ok := op["responses"].(map[string]any); ok {
				codes := make([]string, 0, len(rs))
				for c := range rs {
					if strings.HasPrefix(c, "2") {
						codes = append(codes, c)
					}
				}
				sort.Strings(codes)
				if len(codes) > 0 {
					if r, ok := rs[codes[0]].(map[string]any); ok {
						info.Response = jsonSchema(root, resolve(root, r))
					}
				}
			}
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// resolve follows a local $ref ("#/a/b/c") in root.
func resolve(root, node map[string]any) map[string]any {
	for i := 0; i < 8; i++ {
		ref, ok := node["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return node
		}
		var cur any = root
		for _, seg := range strings.Split(ref[2:], "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				return node
			}
			cur = m[seg]
		}
		next, ok := cur.(map[string]any)
		if !ok {
			return node
		}
		node = next
	}
	return node
}

func jsonSchema(root, holder map[string]any) json.RawMessage {
	content, _ := holder["content"].(map[string]any)
	mt, _ := content["application/json"].(map[string]any)
	sch, _ := mt["schema"].(map[string]any)
	if sch == nil {
		return nil
	}
	b, _ := json.Marshal(resolve(root, sch))
	return b
}

func (c *Connector) loadSpec(ctx context.Context, cfg *config) ([]byte, error) {
	if cfg.openAPISpec != "" {
		return []byte(cfg.openAPISpec), nil
	}
	if cfg.openAPIURL == "" {
		return nil, errors.New("set openApiSpec (inline JSON) or openApiUrl in the connection settings")
	}
	req, err := nethttp.NewRequestWithContext(ctx, "GET", cfg.openAPIURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client(cfg).Do(req)
	if err != nil {
		return nil, errors.New(redact(err.Error(), cfg))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetching OpenAPI document: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// Browse imports an OpenAPI 3 document (connection settings openApiSpec or openApiUrl): without
// object_type_id it lists the operations; with one it returns the request/response profiles.
func (c *Connector) Browse(ctx context.Context, req *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	cfg, err := parseConfig(req.GetConnection())
	if err != nil {
		return nil, err
	}
	if cfg.openAPISpec == "" && cfg.openAPIURL == "" {
		return nil, connector.ErrNotImplemented
	}
	raw, err := c.loadSpec(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ops, err := ParseOpenAPI(raw)
	if err != nil {
		return nil, err
	}
	resp := &connectorv1.BrowseResponse{}
	for _, o := range ops {
		if req.GetCustomType() != "" && !strings.EqualFold(req.GetCustomType(), o.Method) {
			continue
		}
		if req.GetObjectTypeId() == "" {
			label := o.Summary
			if label == "" {
				label = o.Method + " " + o.Path
			}
			resp.ObjectTypes = append(resp.ObjectTypes, &connectorv1.ObjectType{Id: o.ID, Label: label})
			continue
		}
		if o.ID == req.GetObjectTypeId() {
			prof := func(b json.RawMessage) *connectorv1.Profile {
				if b == nil {
					return nil
				}
				return &connectorv1.Profile{Format: connectorv1.Profile_FORMAT_JSON_SCHEMA, Definition: b}
			}
			resp.RequestProfile, resp.ResponseProfile = prof(o.RequestBody), prof(o.Response)
			return resp, nil
		}
	}
	if req.GetObjectTypeId() != "" {
		return nil, fmt.Errorf("operation %q not found in OpenAPI document", req.GetObjectTypeId())
	}
	return resp, nil
}
