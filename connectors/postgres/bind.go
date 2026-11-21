// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Statement is the input document: SQL text plus bound parameters. The SQL text is sent to the
// server as-is; values are only ever bound as parameters (positional $1.. with an array, or
// named @name with an object), never concatenated into the SQL.
type Statement struct {
	SQL    string          `json:"sql"`
	Params json.RawMessage `json:"params,omitempty"`
}

// errInput marks a malformed input document (reported as APPLICATION_ERROR).
var errInput = errors.New("invalid input document")

// ParseStatement decodes one input document.
func ParseStatement(data []byte) (sql string, args []any, err error) {
	var st Statement
	if err := json.Unmarshal(data, &st); err != nil {
		return "", nil, fmt.Errorf("%w: %v", errInput, err)
	}
	if strings.TrimSpace(st.SQL) == "" {
		return "", nil, fmt.Errorf("%w: sql is required", errInput)
	}
	args, err = BindParams(st.Params)
	if err != nil {
		return "", nil, err
	}
	return st.SQL, args, nil
}

// BindParams converts a JSON array (positional) or object (named, @name) into pgx arguments.
// Numbers keep integer precision (int64) when they are integral.
func BindParams(raw json.RawMessage) ([]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: params: %v", errInput, err)
	}
	switch p := v.(type) {
	case []any:
		out := make([]any, len(p))
		for i, e := range p {
			c, err := convertParam(e)
			if err != nil {
				return nil, fmt.Errorf("%w: params[%d]: %v", errInput, i, err)
			}
			out[i] = c
		}
		return out, nil
	case map[string]any:
		named := pgx.NamedArgs{}
		for k, e := range p {
			c, err := convertParam(e)
			if err != nil {
				return nil, fmt.Errorf("%w: params.%s: %v", errInput, k, err)
			}
			named[k] = c
		}
		return []any{named}, nil
	default:
		return nil, fmt.Errorf("%w: params must be an array or an object", errInput)
	}
}

func convertParam(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, err
		}
		return f, nil
	case map[string]any, []any:
		// objects and arrays are bound as JSON (json/jsonb columns)
		b, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	default:
		return v, nil // string, bool, nil
	}
}

// RowToDocument maps one row to a JSON document {column: value}.
func RowToDocument(cols []string, vals []any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, c := range cols {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(c)
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(normalizeValue(vals[i]))
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", c, err)
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case [16]byte:
		return fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
	case pgtype.Numeric:
		if !x.Valid {
			return nil
		}
		b, err := x.MarshalJSON()
		if err != nil {
			return nil
		}
		return json.RawMessage(b)
	case []byte:
		return x // base64 in JSON
	case pgtype.Interval, pgtype.Time:
		return fmt.Sprint(x)
	default:
		return v
	}
}

// MapError maps a database or driver error to a document status. Business-level rejections
// (syntax, data, constraint, permission classes) are APPLICATION_ERROR; connection, resource,
// shutdown and context errors are FAILURE. Messages never contain the password.
func MapError(err error, secrets ...string) (connectorv1.DocumentStatus, string) {
	if err == nil {
		return connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS, ""
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		msg := fmt.Sprintf("%s (SQLSTATE %s)", pe.Message, pe.Code)
		if len(pe.Code) >= 2 {
			switch pe.Code[:2] {
			case "22", "23", "42", "21", "28", "2B", "2D", "3D", "3F", "44", "P0":
				return connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, redact(msg, secrets...)
			}
			if pe.Code == "40001" || pe.Code == "40P01" { // serialization / deadlock: retryable
				return connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, redact(msg, secrets...)
			}
		}
		return connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, redact(msg, secrets...)
	}
	if errors.Is(err, errInput) {
		return connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, redact(err.Error(), secrets...)
	}
	return connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, redact(err.Error(), secrets...)
}
