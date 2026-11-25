// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// fakeTx records the calls made on a transaction; Query fails with err.
type fakeTx struct {
	pgx.Tx
	err        error
	queried    []string
	rolledBack bool
}

func (f *fakeTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.queried = append(f.queried, sql)
	return nil, f.err
}

func (*fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (*fakeTx) Conn() *pgx.Conn { return nil }

func (f *fakeTx) Rollback(context.Context) error { f.rolledBack = true; return nil }

type fakeBeginner struct {
	tx   *fakeTx
	opts []pgx.TxOptions
}

func (f *fakeBeginner) BeginTx(_ context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	f.opts = append(f.opts, o)
	return f.tx, nil
}

func TestTxOptions(t *testing.T) {
	if got := txOptions(true); got.AccessMode != pgx.ReadOnly {
		t.Errorf("QUERY must be READ ONLY, got %q", got.AccessMode)
	}
	if got := txOptions(false); got.AccessMode != "" {
		t.Errorf("non-QUERY must keep the server default, got %q", got.AccessMode)
	}
}

func TestQueryRunsInReadOnlyTxAndRollsBack(t *testing.T) {
	ro := &pgconn.PgError{Code: "25006", Message: "cannot execute INSERT in a read-only transaction", Detail: "host=db.internal"}
	b := &fakeBeginner{tx: &fakeTx{err: ro}}
	o := runReadOnly(context.Background(), b, &inDoc{id: "0", data: []byte(`{"sql":"insert into t values (1)"}`)}, 10, &queryBuffer{}, "pw")
	if len(b.opts) != 1 || b.opts[0].AccessMode != pgx.ReadOnly {
		t.Fatalf("expected one READ ONLY BeginTx, got %+v", b.opts)
	}
	if !b.tx.rolledBack {
		t.Error("transaction must be rolled back")
	}
	if o.status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR {
		t.Errorf("status = %v, want APPLICATION_ERROR", o.status)
	}
	if !strings.Contains(o.message, "25006") || strings.Contains(o.message, "db.internal") || strings.Contains(o.message, "INSERT") {
		t.Errorf("message must name the SQLSTATE only: %q", o.message)
	}
}

func TestMapErrorReadOnly(t *testing.T) {
	st, msg := MapError(&pgconn.PgError{Code: "25006", Message: "cannot execute UPDATE in a read-only transaction"})
	if st != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR || !strings.Contains(msg, "read-only") || !strings.Contains(msg, "25006") {
		t.Errorf("got %v %q", st, msg)
	}
	// other class-25 errors stay technical failures
	if st, _ := MapError(&pgconn.PgError{Code: "25P02", Message: "aborted"}); st != connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE {
		t.Errorf("25P02 must stay FAILURE, got %v", st)
	}
}

func TestQueryRejectsTransactionCommandsBeforeExecution(t *testing.T) {
	tx := &fakeTx{}
	for _, input := range []string{
		`{"sql":"-- notice\n /* outer /* nested */ comment */ ; CoMmIt"}`,
		`{"sql":"set transaction read only"}`,
	} {
		result := runStatement(t.Context(), tx, &inDoc{data: []byte(input)}, true, 10, &queryBuffer{}, "")
		if result.status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR || len(tx.queried) != 0 {
			t.Fatalf("expected application error before execution, got %+v", result)
		}
	}
}
