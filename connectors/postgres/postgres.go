// SPDX-License-Identifier: Apache-2.0

// Package postgres is the native PostgreSQL connector (pgx/v5).
//
// Input documents are JSON {"sql": "...", "params": [..] | {..}}; values are always bound as
// parameters. QUERY emits one output document per row; CREATE/UPDATE/DELETE/EXECUTE run a
// statement and report rows affected in the tracked property "rows_affected".
// Options: transaction (bool, all documents in one transaction, all-or-nothing),
// maxRows (default 10000), timeoutSeconds (per statement, default 30).
//
// SECURITY: by design the input document carries arbitrary SQL that is executed as-is with the
// privileges of the configured database user. Values are bound as parameters, but the statement
// text itself is not restricted (no allow-list). QUERY runs inside a READ ONLY transaction
// enforced by the database, so a write attempted in a QUERY step is rejected (SQLSTATE 25006,
// APPLICATION_ERROR for that document). This is a guard against accidental writes, not a
// complete boundary: it does not limit what data the user may read, it does not apply to
// CREATE/UPDATE/DELETE/EXECUTE (those run any statement the user may run, including DDL and
// COPY ... PROGRAM-class features if granted), and a statement that deliberately switches the
// transaction back to read-write may defeat it. Connect with a dedicated least-privilege
// database account (a read-only role for QUERY steps) and control who may build flows that use
// the connection.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	defaultMaxRows = 10000
	defaultTimeout = 30 * time.Second
	// CustomTypeStatement is the EXECUTE custom type for arbitrary statements.
	CustomTypeStatement = "STATEMENT"
)

// maxPools bounds the number of cached connection pools; the least recently used idle pool is
// closed when the bound is exceeded.
const maxPools = 8

// Connector implements connector.Connector for PostgreSQL.
type Connector struct {
	// ConfigurePool optionally sets host-provided connection policy before the pool opens.
	// Set it before using the connector.
	ConfigurePool func(*pgxpool.Config) error
	// MaxQueryResultBytes optionally bounds the bytes of encoded rows buffered at once.
	// Set it before using the connector.
	MaxQueryResultBytes int

	mu    sync.Mutex
	pools map[string]*poolEntry
	tick  uint64
}

// poolEntry is a cached pool with a reference count; an evicted pool is closed once unused.
type poolEntry struct {
	pool    *pgxpool.Pool
	refs    int
	used    uint64
	evicted bool
}

var _ connector.Connector = (*Connector)(nil)

// New returns the PostgreSQL connector.
func New() *Connector { return &Connector{pools: map[string]*poolEntry{}} }

// Close closes all pools.
func (c *Connector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.pools {
		e.pool.Close()
		delete(c.pools, k)
	}
}

// pool returns a cached pool for the connection, creating it if needed. The caller must call
// release when done so an evicted pool can be closed.
func (c *Connector) pool(ctx context.Context, cc *connectorv1.ConnectionConfig) (*pgxpool.Pool, *Config, func(), error) {
	cfg, err := ParseConfig(cc)
	if err != nil {
		return nil, nil, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cfg.key()
	e, ok := c.pools[key]
	if !ok {
		p, err := c.openPool(ctx, cfg)
		if err != nil {
			return nil, cfg, nil, err
		}
		e = &poolEntry{pool: p}
		c.pools[key] = e
		c.evictLocked(key)
	}
	e.refs++
	c.tick++
	e.used = c.tick
	release := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		e.refs--
		if e.evicted && e.refs == 0 {
			e.pool.Close()
		}
	}
	return e.pool, cfg, release, nil
}

// evictLocked removes least-recently-used pools beyond maxPools, never the one just created.
func (c *Connector) evictLocked(keep string) {
	for len(c.pools) > maxPools {
		var oldest string
		var oldestUsed uint64
		for k, e := range c.pools {
			if k == keep {
				continue
			}
			if oldest == "" || e.used < oldestUsed {
				oldest, oldestUsed = k, e.used
			}
		}
		e := c.pools[oldest]
		delete(c.pools, oldest)
		e.evicted = true
		if e.refs == 0 {
			e.pool.Close()
		}
	}
}

// openPool applies the host policy before either Test or Execute can connect.
func (c *Connector) openPool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	pc, err := cfg.poolConfig()
	if err != nil {
		return nil, err
	}
	if c.ConfigurePool != nil {
		if err := c.ConfigurePool(pc); err != nil {
			return nil, errors.New(redact(err.Error(), cfg.Password))
		}
	}
	p, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, errors.New(redact(err.Error(), cfg.Password))
	}
	return p, nil
}

const connectionSchema = `{
  "type": "object",
  "required": ["host", "database", "user"],
  "properties": {
    "host": {"type": "string"},
    "port": {"type": "integer", "default": 5432},
    "database": {"type": "string"},
    "user": {"type": "string"},
    "password": {"type": "string", "format": "secret-ref", "description": "secret reference, resolved in memory by the runtime"},
    "sslMode": {"type": "string", "enum": ["disable", "require", "verify-ca", "verify-full"], "default": "require"},
    "sslRootCert": {"type": "string", "description": "path to the CA bundle"},
    "poolMaxConns": {"type": "integer", "minimum": 1, "maximum": 100, "default": 4},
    "connectTimeoutSeconds": {"type": "integer", "default": 10}
  }
}`

const optionsSchema = `{
  "type": "object",
  "properties": {
    "transaction": {"type": "boolean", "default": false},
    "maxRows": {"type": "integer", "default": 10000},
    "timeoutSeconds": {"type": "integer", "default": 30}
  }
}`

func mustStruct(js string) *structpb.Struct {
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		panic(err)
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}

// Describe returns the descriptor.
func (*Connector) Describe(context.Context) (*connectorv1.ConnectorDescriptor, error) {
	opts := mustStruct(optionsSchema)
	op := func(t connectorv1.OperationType, custom string) *connectorv1.OperationDescriptor {
		return &connectorv1.OperationDescriptor{Type: t, CustomType: custom, OptionsSchema: opts}
	}
	return &connectorv1.ConnectorDescriptor{
		Id:               "postgres",
		Version:          "0.1.0",
		Host:             connectorv1.Host_HOST_GO_NATIVE,
		DisplayName:      "PostgreSQL",
		ConnectionSchema: mustStruct(connectionSchema),
		Operations: []*connectorv1.OperationDescriptor{
			op(connectorv1.OperationType_OPERATION_TYPE_QUERY, ""),
			op(connectorv1.OperationType_OPERATION_TYPE_CREATE, ""),
			op(connectorv1.OperationType_OPERATION_TYPE_UPDATE, ""),
			op(connectorv1.OperationType_OPERATION_TYPE_DELETE, ""),
			op(connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomTypeStatement),
		},
		TrackedProperties: []string{"rows_affected", "row_count"},
		Capabilities:      &connectorv1.Capabilities{Streaming: false, MaxBatchSize: 1000, Transactional: true},
	}, nil
}

// Browse is not supported yet.
func (*Connector) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}

// Test connects and runs SELECT 1. The diagnostic never contains the password.
func (c *Connector) Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	cfg, err := ParseConfig(req.GetConnection())
	if err != nil {
		return &connectorv1.TestResponse{Ok: false, Diagnostic: err.Error()}, nil
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ConnectTimeoutS)*time.Second)
	defer cancel()
	p, err := c.openPool(tctx, cfg)
	if err != nil {
		return &connectorv1.TestResponse{Ok: false, Diagnostic: redact(err.Error(), cfg.Password)}, nil
	}
	defer p.Close()
	var one int
	if err := p.QueryRow(tctx, "select 1").Scan(&one); err != nil {
		_, msg := MapError(err, cfg.Password)
		return &connectorv1.TestResponse{Ok: false, Diagnostic: "connection failed: " + msg}, nil
	}
	return &connectorv1.TestResponse{Ok: true, Diagnostic: fmt.Sprintf("connected to %s:%d/%s (sslmode=%s)", cfg.Host, cfg.Port, cfg.Database, cfg.SSLMode)}, nil
}

type inDoc struct {
	id    string
	props map[string]string
	data  []byte
}

type docOutcome struct {
	status  connectorv1.DocumentStatus
	message string
	tracked map[string]string
	rows    [][]byte // QUERY output documents
}

// Execute runs the batch. Per-document errors are results, not stream errors.
func (c *Connector) Execute(stream connectorv1.ConnectorService_ExecuteServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return errors.New("postgres: first message must be ExecuteStart")
	}

	switch ot, ct := start.GetOperationType(), start.GetCustomType(); {
	case ot == connectorv1.OperationType_OPERATION_TYPE_QUERY, ot == connectorv1.OperationType_OPERATION_TYPE_CREATE,
		ot == connectorv1.OperationType_OPERATION_TYPE_UPDATE, ot == connectorv1.OperationType_OPERATION_TYPE_DELETE,
		ot == connectorv1.OperationType_OPERATION_TYPE_EXECUTE && ct == CustomTypeStatement:
	default:
		return status.Errorf(codes.InvalidArgument, "postgres: unsupported operation %v %q", ot, ct)
	}

	var docs []*inDoc
	byID := map[string]*inDoc{}
recv:
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch x := m.Msg.(type) {
		case *connectorv1.ExecuteRequest_Chunk:
			ch := x.Chunk
			d, ok := byID[ch.GetDocumentId()]
			if !ok {
				d = &inDoc{id: ch.GetDocumentId(), props: ch.GetProperties()}
				byID[d.id] = d
				docs = append(docs, d)
			}
			d.data = append(d.data, ch.GetData()...)
		case *connectorv1.ExecuteRequest_End:
			break recv
		}
	}

	opts := start.GetOptions().AsMap()
	tx, _ := opts["transaction"].(bool)
	maxRows := defaultMaxRows
	if v, ok := opts["maxRows"].(float64); ok && v > 0 {
		maxRows = int(v)
	}
	timeout := defaultTimeout
	if v, ok := opts["timeoutSeconds"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	isQuery := start.GetOperationType() == connectorv1.OperationType_OPERATION_TYPE_QUERY

	pool, cfg, release, err := c.pool(ctx, start.GetConnection())
	if err != nil {
		// whole batch failed for a technical/config reason
		if cfg == nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return err
	}
	defer release()

	send := func(d *inDoc, o docOutcome) error {
		var ids []string
		for i, row := range o.rows {
			id := d.id + "/" + strconv.Itoa(i)
			ids = append(ids, id)
			if err := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{
				Output: &connectorv1.DocumentChunk{DocumentId: id, Data: row, Last: true},
			}}); err != nil {
				return err
			}
		}
		return stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{
			Result: &connectorv1.DocumentResult{
				DocumentId: d.id, Status: o.status, Message: o.message, Tracked: o.tracked, OutputDocumentIds: ids,
			},
		}})
	}

	if !tx {
		for _, d := range docs {
			sctx, cancel := context.WithTimeout(ctx, timeout)
			budget := queryBuffer{limit: c.MaxQueryResultBytes}
			var o docOutcome
			if isQuery {
				o = runReadOnly(sctx, pool, d, maxRows, &budget, cfg.Password)
			} else {
				o = runStatement(sctx, pool, d, false, maxRows, &budget, cfg.Password)
			}
			cancel()
			if err := send(d, o); err != nil {
				return err
			}
		}
		return nil
	}

	// Transaction mode: all documents commit together or none does.
	outcomes := make([]docOutcome, len(docs))
	fail := func(_ int, msg string) {
		for i := range docs {
			outcomes[i] = docOutcome{status: connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, message: msg}
		}
	}
	t, err := beginStatementTx(ctx, pool, isQuery)
	if err != nil {
		_, msg := MapError(err, cfg.Password)
		fail(-1, "transaction not started: "+msg)
	} else {
		failedAt := -1
		budget := queryBuffer{limit: c.MaxQueryResultBytes}
		for i, d := range docs {
			sctx, cancel := context.WithTimeout(ctx, timeout)
			o := runStatement(sctx, t, d, isQuery, maxRows, &budget, cfg.Password)
			cancel()
			outcomes[i] = o
			if o.status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
				failedAt = i
				break
			}
		}
		if failedAt >= 0 {
			_ = t.Rollback(ctx)
			for i := range docs {
				if i != failedAt {
					outcomes[i] = docOutcome{status: connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE, message: "transaction rolled back: document " + docs[failedAt].id + " failed"}
				}
			}
		} else if err := t.Commit(ctx); err != nil {
			_, msg := MapError(err, cfg.Password)
			fail(-1, "commit failed, transaction rolled back: "+msg)
		}
	}
	for i, d := range docs {
		if err := send(d, outcomes[i]); err != nil {
			return err
		}
	}
	return nil
}

// readOnlyTx is the transaction mode used for every QUERY: the database itself rejects writes.
var readOnlyTx = pgx.TxOptions{AccessMode: pgx.ReadOnly}

// txOptions returns the transaction options for an operation: READ ONLY for QUERY, the server
// default otherwise.
func txOptions(isQuery bool) pgx.TxOptions {
	if isQuery {
		return readOnlyTx
	}
	return pgx.TxOptions{}
}

// beginner is satisfied by *pgxpool.Pool (the seam used by unit tests).
type beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Establish a snapshot before user SQL runs. PostgreSQL then refuses to switch
// this read-only transaction to read-write, including through configuration functions.
func beginStatementTx(ctx context.Context, b beginner, isQuery bool) (pgx.Tx, error) {
	t, err := b.BeginTx(ctx, txOptions(isQuery))
	if err != nil || !isQuery {
		return t, err
	}
	if _, err := t.Exec(ctx, "select 1"); err != nil {
		_ = t.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return t, nil
}

// runReadOnly runs one document in its own READ ONLY transaction and always rolls it back (a
// QUERY must not change anything, so there is nothing to commit).
func runReadOnly(ctx context.Context, b beginner, d *inDoc, maxRows int, budget *queryBuffer, secret string) docOutcome {
	t, err := beginStatementTx(ctx, b, true)
	if err != nil {
		st, msg := MapError(err, secret)
		return docOutcome{status: st, message: "transaction not started: " + msg}
	}
	o := runStatement(ctx, t, d, true, maxRows, budget, secret)
	_ = t.Rollback(context.WithoutCancel(ctx))
	return o
}

// querier is satisfied by *pgxpool.Pool and pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// queryCommand reads the first SQL keyword, skipping PostgreSQL whitespace,
// empty statements and comments. Block comments can nest.
func queryCommand(sql string) string {
	for {
		sql = strings.TrimLeft(sql, " \t\n\r\f\v;")
		switch {
		case strings.HasPrefix(sql, "--"):
			end := strings.IndexAny(sql, "\r\n")
			if end < 0 {
				return ""
			}
			sql = sql[end:]
		case strings.HasPrefix(sql, "/*"):
			depth := 1
			sql = sql[2:]
			for depth > 0 && len(sql) > 0 {
				switch {
				case strings.HasPrefix(sql, "/*"):
					depth++
					sql = sql[2:]
				case strings.HasPrefix(sql, "*/"):
					depth--
					sql = sql[2:]
				default:
					sql = sql[1:]
				}
			}
		default:
			end := 0
			for end < len(sql) && ((sql[end] >= 'A' && sql[end] <= 'Z') || (sql[end] >= 'a' && sql[end] <= 'z')) {
				end++
			}
			return strings.ToUpper(sql[:end])
		}
	}
}

// Query rows are retained until they can be emitted, so a transaction shares a
// single budget across all its documents.
type queryBuffer struct {
	limit int
	used  int
}

func (b *queryBuffer) reserve(bytes int) bool {
	if b.limit <= 0 {
		return true
	}
	if bytes > b.limit-b.used {
		return false
	}
	b.used += bytes
	return true
}

func runStatement(ctx context.Context, q querier, d *inDoc, isQuery bool, maxRows int, budget *queryBuffer, secret string) docOutcome {
	sql, args, err := ParseStatement(d.data)
	if err != nil {
		st, msg := MapError(err, secret)
		return docOutcome{status: st, message: msg}
	}
	if isQuery {
		switch queryCommand(sql) {
		case "ABORT", "BEGIN", "COMMIT", "DISCARD", "END", "PREPARE", "RELEASE", "RESET", "ROLLBACK", "SAVEPOINT", "SET", "START":
			return docOutcome{status: connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
				message: "QUERY cannot change transaction or session settings"}
		}
		// Extended protocol rejects multiple SQL statements in one document.
		if tx, ok := q.(pgx.Tx); ok && tx.Conn() != nil &&
			tx.Conn().Config().DefaultQueryExecMode == pgx.QueryExecModeSimpleProtocol {
			args = append([]any{pgx.QueryExecModeDescribeExec}, args...)
		}
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		st, msg := MapError(err, secret)
		return docOutcome{status: st, message: msg}
	}
	defer rows.Close()
	out := docOutcome{status: connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS, tracked: map[string]string{}}
	cols := make([]string, 0)
	for _, f := range rows.FieldDescriptions() {
		cols = append(cols, f.Name)
	}
	n := 0
	for rows.Next() {
		n++
		if !isQuery {
			continue // statements with RETURNING: rows are counted, not emitted
		}
		if n > maxRows {
			rows.Close()
			return docOutcome{status: connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
				message: fmt.Sprintf("result exceeds maxRows (%d)", maxRows)}
		}
		vals, err := rows.Values()
		if err == nil {
			var doc []byte
			if doc, err = RowToDocument(cols, vals); err == nil {
				if !budget.reserve(len(doc)) {
					return docOutcome{status: connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
						message: "query rows exceed the test run buffer limit"}
				}
				out.rows = append(out.rows, doc)
				continue
			}
		}
		st, msg := MapError(err, secret)
		return docOutcome{status: st, message: msg}
	}
	if err := rows.Err(); err != nil {
		st, msg := MapError(err, secret)
		return docOutcome{status: st, message: msg}
	}
	rows.Close()
	out.tracked["row_count"] = strconv.Itoa(n)
	out.tracked["rows_affected"] = strconv.FormatInt(rows.CommandTag().RowsAffected(), 10)
	return out
}
