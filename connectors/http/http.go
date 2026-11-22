// SPDX-License-Identifier: Apache-2.0

// Package http is the native HTTP/REST connector.
//
// Operation: EXECUTE with the HTTP method as custom type (GET, POST, PUT, PATCH, DELETE).
// Each input document is one request; its payload is the request body and its properties tune
// the request:
//
//	http.method        overrides the method (otherwise ExecuteStart.custom_type)
//	http.path          path relative to baseUrl (otherwise options.path)
//	http.query.<name>  query parameter
//	http.header.<name> request header
//
// ExecuteStart.options: path, query{}, headers{}, bodyTemplate, retries, retryBackoffMs.
// {{name}} placeholders in path, header values and bodyTemplate are filled from document
// properties; {{data}} in bodyTemplate is the document payload.
//
// Status mapping: 2xx SUCCESS; 4xx APPLICATION_ERROR (the target rejected the document), except
// 408 and 429 which are retried and then reported as FAILURE; 5xx and transport errors FAILURE.
// 3xx is followed by the Go client; an unfollowed 3xx is APPLICATION_ERROR. The response body is
// emitted as an output document "<id>.response"; a body larger than 32 MiB is a FAILURE, never a
// silently truncated SUCCESS. Redirects are capped at 10 and cross-host redirects drop the
// configured headers and API key.
//
// SSRF: baseUrl and the optional openApiUrl are operator-supplied and fetched from wherever the
// connector runs, including internal networks and metadata endpoints. Only operators who may
// configure connections should be able to set them; restrict egress at the network layer.
package http

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// Version of the connector.
const Version = "0.1.0"

// maxResponseBody bounds how much of a response is kept per document.
const maxResponseBody = 32 << 20

var methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// Connector implements connector.Connector for HTTP/REST.
type Connector struct {
	// Client, if set, is used instead of a client built from the connection settings (tests).
	Client *nethttp.Client
}

var _ connector.Connector = (*Connector)(nil)

// New returns the HTTP connector.
func New() *Connector { return &Connector{} }

// Describe returns the connector descriptor (mirrors connector.yaml).
func (*Connector) Describe(context.Context) (*connectorv1.ConnectorDescriptor, error) {
	schema, err := structpb.NewStruct(connectionSchema)
	if err != nil {
		return nil, err
	}
	var ops []*connectorv1.OperationDescriptor
	for _, m := range methods {
		ops = append(ops, &connectorv1.OperationDescriptor{
			Type: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: m, SupportsBrowse: true,
		})
	}
	return &connectorv1.ConnectorDescriptor{
		Id:                "http",
		Version:           Version,
		Host:              connectorv1.Host_HOST_GO_NATIVE,
		DisplayName:       "HTTP / REST",
		ConnectionSchema:  schema,
		Operations:        ops,
		TrackedProperties: []string{"status_code", "url"},
		Capabilities:      &connectorv1.Capabilities{Streaming: true},
	}, nil
}

var connectionSchema = map[string]any{
	"$schema":  "https://json-schema.org/draft/2020-12/schema",
	"type":     "object",
	"required": []any{"baseUrl"},
	"properties": map[string]any{
		"baseUrl": map[string]any{"type": "string", "format": "uri"},
		"auth": map[string]any{"type": "object", "properties": map[string]any{
			"type":        map[string]any{"enum": []any{"none", "basic", "bearer", "apikey"}},
			"password":    map[string]any{"type": "string", "x-nurtuo-secret": true},
			"token":       map[string]any{"type": "string", "x-nurtuo-secret": true},
			"apiKeyValue": map[string]any{"type": "string", "x-nurtuo-secret": true},
		}},
		"timeoutMs":   map[string]any{"type": "integer", "minimum": 1},
		"retries":     map[string]any{"type": "integer", "minimum": 0},
		"openApiSpec": map[string]any{"type": "string", "description": "Inline OpenAPI 3 document (JSON text) used by Browse"},
		"openApiUrl":  map[string]any{"type": "string", "format": "uri", "description": "URL of an OpenAPI 3 document fetched by Browse"},
	},
}

// config is the parsed connection settings.
type config struct {
	baseURL      *url.URL
	authType     string
	username     string
	password     string
	token        string
	keyName      string
	keyValue     string
	keyInQuery   bool
	headers      map[string]string
	timeout      time.Duration
	skipVerify   bool
	testPath     string
	retries      int
	retriesSet   bool
	retryBackoff time.Duration
	openAPISpec  string
	openAPIURL   string
}

func parseConfig(cc *connectorv1.ConnectionConfig) (*config, error) {
	s := cc.GetSettings().AsMap()
	c := &config{timeout: 30 * time.Second, retries: 2, retryBackoff: 200 * time.Millisecond, testPath: "/", headers: map[string]string{}}
	base, _ := s["baseUrl"].(string)
	if base == "" {
		return nil, errors.New("baseUrl is required")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("baseUrl must be an absolute http(s) URL")
	}
	c.baseURL = u
	if a, ok := s["auth"].(map[string]any); ok {
		c.authType = strings.ToLower(str(a["type"]))
		c.username, c.password, c.token = str(a["username"]), str(a["password"]), str(a["token"])
		c.keyName, c.keyValue = str(a["apiKeyName"]), str(a["apiKeyValue"])
		c.keyInQuery = strings.EqualFold(str(a["apiKeyIn"]), "query")
	}
	switch c.authType {
	case "", "none":
	case "basic":
		if c.username == "" {
			return nil, errors.New("auth.username is required for basic auth")
		}
	case "bearer":
		if c.token == "" {
			return nil, errors.New("auth.token is required for bearer auth")
		}
	case "apikey":
		if c.keyName == "" || c.keyValue == "" {
			return nil, errors.New("auth.apiKeyName and auth.apiKeyValue are required for apikey auth")
		}
	default:
		return nil, fmt.Errorf("unsupported auth.type %q", c.authType)
	}
	if h, ok := s["headers"].(map[string]any); ok {
		for k, v := range h {
			c.headers[k] = str(v)
		}
	}
	if v, ok := num(s["timeoutMs"]); ok && v > 0 {
		c.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := num(s["retries"]); ok && v >= 0 {
		c.retries, c.retriesSet = v, true
	}
	if v, ok := num(s["retryBackoffMs"]); ok && v >= 0 {
		c.retryBackoff = time.Duration(v) * time.Millisecond
	}
	c.skipVerify, _ = s["insecureSkipVerify"].(bool)
	if p := str(s["testPath"]); p != "" {
		c.testPath = p
	}
	c.openAPISpec, c.openAPIURL = str(s["openApiSpec"]), str(s["openApiUrl"])
	return c, nil
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) (int, bool) {
	f, ok := v.(float64)
	return int(f), ok
}

func (c *Connector) client(cfg *config) *nethttp.Client {
	if c.Client != nil {
		return c.Client
	}
	tr := nethttp.DefaultTransport.(*nethttp.Transport).Clone()
	if cfg.skipVerify {
		tr.TLSClientConfig = insecureTLS()
	}
	return &nethttp.Client{Transport: tr, CheckRedirect: checkRedirect(cfg)}
}

// Test issues a GET to testPath. Any HTTP response (even 404) proves reachability; 401/403 are
// reported as failed authentication. The diagnostic never contains credentials.
func (c *Connector) Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	cfg, err := parseConfig(req.GetConnection())
	if err != nil {
		return &connectorv1.TestResponse{Ok: false, Diagnostic: "invalid configuration: " + err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	hr, err := buildRequest(ctx, cfg, "GET", cfg.testPath, nil, nil, nil, nil)
	if err != nil {
		return &connectorv1.TestResponse{Ok: false, Diagnostic: "invalid test path: " + err.Error()}, nil
	}
	resp, err := c.client(cfg).Do(hr)
	if err != nil {
		return &connectorv1.TestResponse{Ok: false, Diagnostic: "request failed: " + redact(err.Error(), cfg)}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return &connectorv1.TestResponse{Ok: false, Diagnostic: fmt.Sprintf("authentication rejected (HTTP %d)", resp.StatusCode)}, nil
	case resp.StatusCode >= 500:
		return &connectorv1.TestResponse{Ok: false, Diagnostic: fmt.Sprintf("server error (HTTP %d)", resp.StatusCode)}, nil
	}
	return &connectorv1.TestResponse{Ok: true, Diagnostic: fmt.Sprintf("reachable (HTTP %d)", resp.StatusCode)}, nil
}

func redact(s string, cfg *config) string {
	for _, secret := range []string{cfg.password, cfg.token, cfg.keyValue} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

var placeholder = regexp.MustCompile(`\{\{\s*([^{}\s]+)\s*\}\}`)

// render fills {{name}} placeholders from vars; unknown names are an error.
func render(tpl string, vars map[string]string, esc func(string) string) (string, error) {
	var missing []string
	out := placeholder.ReplaceAllStringFunc(tpl, func(m string) string {
		name := placeholder.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return ""
		}
		if esc != nil {
			return esc(v)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("template references unknown property %q", missing[0])
	}
	return out, nil
}

// buildRequest assembles a request against cfg.baseURL. path may carry its own query string.
func buildRequest(ctx context.Context, cfg *config, method, path string, query url.Values, hdr map[string]string, body []byte, _ any) (*nethttp.Request, error) {
	u := *cfg.baseURL
	p, rawQuery, _ := strings.Cut(path, "?")
	u.Path = strings.TrimRight(cfg.baseURL.Path, "/") + "/" + strings.TrimLeft(p, "/")
	if strings.HasSuffix(p, "/") || p == "" {
		u.Path = strings.TrimRight(u.Path, "/") + "/"
	}
	u.RawPath = ""
	q := cfg.baseURL.Query()
	if rawQuery != "" {
		pq, err := url.ParseQuery(rawQuery)
		if err != nil {
			return nil, err
		}
		for k, vs := range pq {
			q[k] = vs
		}
	}
	for k, vs := range query {
		q[k] = vs
	}
	if cfg.authType == "apikey" && cfg.keyInQuery {
		q.Set(cfg.keyName, cfg.keyValue)
	}
	u.RawQuery = q.Encode()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	r, err := nethttp.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range cfg.headers {
		r.Header.Set(k, v)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	switch cfg.authType {
	case "basic":
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cfg.username+":"+cfg.password)))
	case "bearer":
		r.Header.Set("Authorization", "Bearer "+cfg.token)
	case "apikey":
		if !cfg.keyInQuery {
			r.Header.Set(cfg.keyName, cfg.keyValue)
		}
	}
	return r, nil
}

// execOptions are the per-step options from ExecuteStart.options.
type execOptions struct {
	path         string
	query        url.Values
	headers      map[string]string
	bodyTemplate string
	hasTemplate  bool
	retries      int
	backoff      time.Duration
}

func parseOptions(cfg *config, st *connectorv1.ExecuteStart) execOptions {
	o := execOptions{query: url.Values{}, headers: map[string]string{}, retries: cfg.retries, backoff: cfg.retryBackoff}
	m := st.GetOptions().AsMap()
	o.path = str(m["path"])
	if q, ok := m["query"].(map[string]any); ok {
		for k, v := range q {
			o.query.Set(k, str(v))
		}
	}
	if h, ok := m["headers"].(map[string]any); ok {
		for k, v := range h {
			o.headers[k] = str(v)
		}
	}
	if t, ok := m["bodyTemplate"].(string); ok {
		o.bodyTemplate, o.hasTemplate = t, true
	}
	if v, ok := num(m["retries"]); ok && v >= 0 {
		o.retries = v
	}
	if v, ok := num(m["retryBackoffMs"]); ok && v >= 0 {
		o.backoff = time.Duration(v) * time.Millisecond
	}
	return o
}

func methodFor(st *connectorv1.ExecuteStart, props map[string]string) (string, error) {
	m := props["http.method"]
	if m == "" {
		m = st.GetCustomType()
	}
	if m == "" {
		switch st.GetOperationType() {
		case connectorv1.OperationType_OPERATION_TYPE_GET, connectorv1.OperationType_OPERATION_TYPE_QUERY:
			m = "GET"
		case connectorv1.OperationType_OPERATION_TYPE_CREATE:
			m = "POST"
		case connectorv1.OperationType_OPERATION_TYPE_UPDATE, connectorv1.OperationType_OPERATION_TYPE_UPSERT:
			m = "PUT"
		case connectorv1.OperationType_OPERATION_TYPE_DELETE:
			m = "DELETE"
		}
	}
	m = strings.ToUpper(m)
	for _, ok := range methods {
		if m == ok {
			return m, nil
		}
	}
	return "", fmt.Errorf("unsupported HTTP method %q", m)
}

type document struct {
	id    string
	props map[string]string
	data  bytes.Buffer
}

// Execute performs one HTTP request per input document and returns one DocumentResult each.
// Only one document is buffered at a time.
func (c *Connector) Execute(stream connectorv1.ConnectorService_ExecuteServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	st := first.GetStart()
	if st == nil {
		return errors.New("http: first message must be ExecuteStart")
	}
	cfg, cfgErr := parseConfig(st.GetConnection())
	var opts execOptions
	if cfgErr == nil {
		opts = parseOptions(cfg, st)
	}
	var cur *document
	flush := func() error {
		if cur == nil {
			return nil
		}
		d := cur
		cur = nil
		var res *connectorv1.DocumentResult
		var out []byte
		if cfgErr != nil {
			res = &connectorv1.DocumentResult{DocumentId: d.id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, Message: "invalid connection: " + cfgErr.Error()}
		} else {
			res, out = c.doDocument(stream.Context(), cfg, opts, st, d)
		}
		if out != nil {
			outID := d.id + ".response"
			res.OutputDocumentIds = []string{outID}
			if err := sendOutput(stream, outID, res.Tracked, out); err != nil {
				return err
			}
		}
		return stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: res}})
	}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return flush()
		}
		if err != nil {
			return err
		}
		if ch := req.GetChunk(); ch != nil {
			if cur != nil && cur.id != ch.GetDocumentId() {
				if err := flush(); err != nil {
					return err
				}
			}
			if cur == nil {
				cur = &document{id: ch.GetDocumentId(), props: map[string]string{}}
			}
			for k, v := range ch.GetProperties() {
				cur.props[k] = v
			}
			cur.data.Write(ch.GetData())
			if ch.GetLast() {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if req.GetEnd() != nil {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

func sendOutput(stream connectorv1.ConnectorService_ExecuteServer, id string, tracked map[string]string, body []byte) error {
	const chunk = 64 * 1024
	props := map[string]string{"http.status": tracked["status_code"], "http.url": tracked["url"]}
	for off := 0; ; off += chunk {
		end := min(off+chunk, len(body))
		ch := &connectorv1.DocumentChunk{DocumentId: id, Data: body[off:end], Last: end == len(body)}
		if off == 0 {
			ch.Properties = props
		}
		if err := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{Output: ch}}); err != nil {
			return err
		}
		if end == len(body) {
			return nil
		}
	}
}

func (c *Connector) doDocument(ctx context.Context, cfg *config, o execOptions, st *connectorv1.ExecuteStart, d *document) (*connectorv1.DocumentResult, []byte) {
	res := &connectorv1.DocumentResult{DocumentId: d.id, Tracked: map[string]string{}}
	fail := func(status connectorv1.DocumentStatus, format string, a ...any) (*connectorv1.DocumentResult, []byte) {
		res.Status, res.Message = status, redact(fmt.Sprintf(format, a...), cfg)
		return res, nil
	}
	method, err := methodFor(st, d.props)
	if err != nil {
		return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, "%v", err)
	}
	vars := map[string]string{}
	for k, v := range d.props {
		vars[k] = v
	}
	vars["data"] = d.data.String()

	pathTpl := d.props["http.path"]
	if pathTpl == "" {
		pathTpl = o.path
	}
	path, err := render(pathTpl, vars, url.PathEscape)
	if err != nil {
		return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, "path: %v", err)
	}
	query := url.Values{}
	for k, v := range o.query {
		query[k] = v
	}
	hdr := map[string]string{}
	for k, v := range o.headers {
		if hdr[k], err = render(v, vars, nil); err != nil {
			return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, "header %s: %v", k, err)
		}
	}
	for k, v := range d.props {
		if n, ok := strings.CutPrefix(k, "http.query."); ok {
			query.Set(n, v)
		} else if n, ok := strings.CutPrefix(k, "http.header."); ok {
			hdr[n] = v
		}
	}
	var body []byte
	if o.hasTemplate {
		s, err := render(o.bodyTemplate, vars, nil)
		if err != nil {
			return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, "bodyTemplate: %v", err)
		}
		body = []byte(s)
	} else if d.data.Len() > 0 {
		body = d.data.Bytes()
	}
	if body != nil && method == "GET" {
		body = nil // GET carries no body
	}

	retries := o.retries
	if (method == "POST" || method == "PATCH") && !cfg.retriesSet && !hasRetryOption(st) {
		retries = 0 // not idempotent: retry only on explicit request
	}
	client := c.client(cfg)
	var lastErr error
	for attempt := 0; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		hr, err := buildRequest(rctx, cfg, method, path, query, hdr, body, nil)
		if err != nil {
			cancel()
			return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, "invalid request: %v", err)
		}
		if _, ok := hdr["Content-Type"]; !ok && body != nil && hr.Header.Get("Content-Type") == "" {
			hr.Header.Set("Content-Type", sniff(body))
		}
		resp, err := client.Do(hr)
		var data []byte
		code := 0
		if err == nil {
			code = resp.StatusCode
			data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
			_ = resp.Body.Close()
			res.Tracked["url"] = safeURL(hr.URL, cfg)
			if err == nil && len(data) > maxResponseBody {
				cancel()
				return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, "response body exceeds the %d byte limit (HTTP %d); not recorded", maxResponseBody, code)
			}
		}
		cancel()
		if err != nil {
			lastErr = err
		} else {
			res.Tracked["status_code"] = fmt.Sprint(code)
			if retryable(code) && attempt < retries {
				lastErr = fmt.Errorf("HTTP %d", code)
			} else {
				return finish(res, code, data, cfg)
			}
		}
		if attempt >= retries {
			return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, "request failed after %d attempt(s): %v", attempt+1, lastErr)
		}
		select {
		case <-ctx.Done():
			return fail(connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, "canceled: %v", ctx.Err())
		case <-time.After(backoffFor(o.backoff, attempt)):
		}
	}
}

// maxBackoff caps the exponential retry delay.
const maxBackoff = 5 * time.Minute

// backoffFor returns base<<attempt capped at maxBackoff, without overflowing.
func backoffFor(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt >= 30 || base > maxBackoff>>attempt {
		return maxBackoff
	}
	return base << attempt
}

// safeURL renders u for recording with userinfo password and the API-key query parameter masked.
func safeURL(u *url.URL, cfg *config) string {
	c := *u
	if cfg.authType == "apikey" && cfg.keyInQuery && cfg.keyName != "" {
		q := c.Query()
		if q.Has(cfg.keyName) {
			q.Set(cfg.keyName, "***")
			c.RawQuery = q.Encode()
		}
	}
	return redact(c.Redacted(), cfg)
}

// maxRedirects bounds redirect chains.
const maxRedirects = 10

// checkRedirect caps redirects and strips configured credentials/custom headers when a redirect
// changes host (Go already drops Authorization/Cookie on cross-domain redirects).
func checkRedirect(cfg *config) func(*nethttp.Request, []*nethttp.Request) error {
	return func(req *nethttp.Request, via []*nethttp.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if req.URL.Host == via[0].URL.Host {
			return nil
		}
		for k := range cfg.headers {
			req.Header.Del(k)
		}
		if cfg.authType == "apikey" && !cfg.keyInQuery {
			req.Header.Del(cfg.keyName)
		}
		req.Header.Del("Authorization")
		if cfg.authType == "apikey" && cfg.keyInQuery {
			q := req.URL.Query()
			q.Del(cfg.keyName)
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
}

func hasRetryOption(st *connectorv1.ExecuteStart) bool {
	_, ok := st.GetOptions().GetFields()["retries"]
	return ok
}

func retryable(code int) bool {
	return code == 408 || code == 429 || code == 502 || code == 503 || code == 504
}

func sniff(b []byte) string {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		return "application/json"
	}
	return "application/octet-stream"
}

func finish(res *connectorv1.DocumentResult, code int, body []byte, cfg *config) (*connectorv1.DocumentResult, []byte) {
	switch {
	case code >= 200 && code < 300:
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
		res.Message = fmt.Sprintf("HTTP %d", code)
	case code >= 400 && code < 500 && code != 408 && code != 429, code >= 300 && code < 400:
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
		res.Message = fmt.Sprintf("HTTP %d: %s", code, snippet(body, cfg))
	default:
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE
		res.Message = fmt.Sprintf("HTTP %d: %s", code, snippet(body, cfg))
	}
	if len(body) == 0 {
		return res, nil
	}
	return res, body
}

func snippet(b []byte, cfg *config) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return redact(s, cfg)
}
