package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/newrelic/go-agent/v3/integrations/nrpgx5"
)

// queryOnlyTracer delegates pgx QUERY and BATCH tracing to nrpgx5 but no-ops
// the CONNECT phase. nrpgx5 v1.3.4's Tracer stores connect-segment state on
// the shared Tracer struct, so concurrent pool connection establishment is a
// data race (caught by -race in the integration suite; no upstream fix
// released). Connect segments are the least interesting datastore telemetry —
// query segments, which carry the latency insight, are unaffected: the query
// paths keep their state on ctx, not on the Tracer.
type queryOnlyTracer struct {
	inner *nrpgx5.Tracer
}

func newQueryOnlyTracer() *queryOnlyTracer {
	return &queryOnlyTracer{inner: nrpgx5.NewTracer()}
}

func (t *queryOnlyTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return t.inner.TraceQueryStart(ctx, conn, data)
}

func (t *queryOnlyTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	t.inner.TraceQueryEnd(ctx, conn, data)
}

func (t *queryOnlyTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	return t.inner.TraceBatchStart(ctx, conn, data)
}

func (t *queryOnlyTracer) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	t.inner.TraceBatchQuery(ctx, conn, data)
}

func (t *queryOnlyTracer) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchEndData) {
	t.inner.TraceBatchEnd(ctx, conn, data)
}
