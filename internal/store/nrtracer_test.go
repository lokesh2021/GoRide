package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// queryOnlyTracer's methods only ever touch ctx/data (never the *pgx.Conn
// argument, mirrored from nrpgx5's own implementation), so they're exercised
// here directly with a nil conn and a background context (no txn attached, so
// newrelic.FromContext resolves nil and every downstream call on it no-ops
// per the go-agent v3 nil-safety guarantee) -- no Postgres connection needed.
func TestQueryOnlyTracerQueryLifecycle(t *testing.T) {
	tr := newQueryOnlyTracer()
	ctx := context.Background()

	ctx = tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	if ctx == nil {
		t.Fatal("TraceQueryStart returned nil context")
	}
	// Must not panic.
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
}

func TestQueryOnlyTracerBatchLifecycle(t *testing.T) {
	tr := newQueryOnlyTracer()
	ctx := context.Background()

	ctx = tr.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
	if ctx == nil {
		t.Fatal("TraceBatchStart returned nil context")
	}
	// Must not panic.
	tr.TraceBatchQuery(ctx, nil, pgx.TraceBatchQueryData{SQL: "SELECT 1"})
	tr.TraceBatchEnd(ctx, nil, pgx.TraceBatchEndData{})
}

func TestNewQueryOnlyTracerWrapsInner(t *testing.T) {
	tr := newQueryOnlyTracer()
	if tr.inner == nil {
		t.Fatal("newQueryOnlyTracer(): inner nrpgx5 tracer is nil")
	}
}
