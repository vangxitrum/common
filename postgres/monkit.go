package postgres

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

type (
	startKey struct{}
	opKey    struct{}
)

// WithOperation names the work the queries made with this context belong to.
//
// The name appears on failure events only, never on a histogram: see the
// cardinality note on the tracer below. It is what turns "something in this
// service is getting deadlocks" into "the invoice reconciliation is".
func WithOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, opKey{}, operation)
}

func operationOf(ctx context.Context) string {
	op, _ := ctx.Value(opKey{}).(string)
	return op
}

// tracer records query, batch, connect and acquire timings for one pool.
//
// One value serves six of pgx's tracer interfaces: ConnConfig.Tracer is typed
// as QueryTracer, but pgx and pgxpool assert it up to the batch, copy, prepare,
// connect and acquire interfaces as well. Those assertions happen inside
// pgxpool.NewWithConfig, which is why the tracer has to be installed before the
// pool is constructed.
//
// The values are created once per pool rather than per query: Scope.DurationVal
// takes a lock and a map lookup, and this runs on every query there is.
type tracer struct {
	name monkit.SeriesTag

	query    *monkit.DurationVal
	batch    *monkit.DurationVal
	batchLen *monkit.IntVal
	copyFrom *monkit.DurationVal
	connect  *monkit.DurationVal
	acquire  *monkit.DurationVal
}

func newTracer(name monkit.SeriesTag) *tracer {
	return &tracer{
		name:     name,
		query:    mon.DurationVal("postgres_query", name),
		batch:    mon.DurationVal("postgres_batch", name),
		batchLen: mon.IntVal("postgres_batch_length", name),
		copyFrom: mon.DurationVal("postgres_copy_from", name),
		connect:  mon.DurationVal("postgres_connect", name),
		acquire:  mon.DurationVal("postgres_acquire", name),
	}
}

// pgx finds these by asserting the one Tracer field up to each interface, so a
// signature that drifted would not fail to compile - it would silently stop
// recording. These assertions are what turn that into a build failure.
var (
	_ pgx.QueryTracer       = (*tracer)(nil)
	_ pgx.BatchTracer       = (*tracer)(nil)
	_ pgx.CopyFromTracer    = (*tracer)(nil)
	_ pgx.PrepareTracer     = (*tracer)(nil)
	_ pgx.ConnectTracer     = (*tracer)(nil)
	_ pgxpool.AcquireTracer = (*tracer)(nil)
)

// started stamps the start time on the context.
//
// The time rides on the context rather than in a map on the tracer because
// pgx's contract does not promise an End for every Start: TraceQueryEnd fires
// from Rows.Close, and a caller who leaks a Rows never closes it. A map entry
// would leak forever; a context value costs one missing sample.
func started(ctx context.Context) context.Context {
	return context.WithValue(ctx, startKey{}, time2.Now(ctx))
}

func elapsed(ctx context.Context) time.Duration {
	start, ok := ctx.Value(startKey{}).(time.Time)
	if !ok {
		return 0
	}
	return time2.Since(ctx, start)
}

// failed records an error, with the two tags a rare event can afford.
func (t *tracer) failed(ctx context.Context, event string, err error) {
	if err == nil {
		return
	}

	tags := []monkit.SeriesTag{t.name, monkit.NewSeriesTag("code", codeOf(err))}
	if op := operationOf(ctx); op != "" {
		tags = append(tags, monkit.NewSeriesTag("op", op))
	}
	mon.Event(event, tags...)
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *tracer) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceQueryStartData,
) context.Context {
	return started(ctx)
}

// TraceQueryEnd implements pgx.QueryTracer.
//
// It measures from the start of the query to the close of its Rows, not server
// time: a caller that holds Rows open while doing other work inflates this. For
// Exec and QueryRow the window is tight; for a manual Next loop, prefer
// pgx.CollectRows, which also closes for you.
//
// pgx.ErrNoRows cannot reach here - Rows.Close fires this with a nil error and
// connRow.Scan synthesizes ErrNoRows afterwards - so the guard below is
// insurance against a future pgx, not a description of today's behavior.
func (t *tracer) TraceQueryEnd(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryEndData,
) {
	t.query.Observe(elapsed(ctx))
	if !errors.Is(data.Err, pgx.ErrNoRows) {
		t.failed(ctx, "postgres_query_failed", data.Err)
	}
}

// TraceBatchStart implements pgx.BatchTracer.
func (t *tracer) TraceBatchStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceBatchStartData,
) context.Context {
	if data.Batch != nil {
		t.batchLen.Observe(int64(data.Batch.Len()))
	}
	return started(ctx)
}

// TraceBatchQuery implements pgx.BatchTracer and deliberately records nothing.
//
// SendBatch sends dozens of statements in one round trip, and one sample per
// member would drown the query histogram. The batch gets its own duration and a
// length distribution instead.
func (t *tracer) TraceBatchQuery(
	context.Context,
	*pgx.Conn,
	pgx.TraceBatchQueryData,
) {
}

// TraceBatchEnd implements pgx.BatchTracer.
func (t *tracer) TraceBatchEnd(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceBatchEndData,
) {
	t.batch.Observe(elapsed(ctx))
	t.failed(ctx, "postgres_batch_failed", data.Err)
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (t *tracer) TraceCopyFromStart(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceCopyFromStartData,
) context.Context {
	return started(ctx)
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (t *tracer) TraceCopyFromEnd(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceCopyFromEndData,
) {
	t.copyFrom.Observe(elapsed(ctx))
	t.failed(ctx, "postgres_copy_from_failed", data.Err)
}

// TracePrepareStart implements pgx.PrepareTracer.
func (t *tracer) TracePrepareStart(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TracePrepareStartData,
) context.Context {
	return ctx
}

// TracePrepareEnd implements pgx.PrepareTracer. Success is not recorded,
// because pgx's automatic statement cache prepares constantly.
func (t *tracer) TracePrepareEnd(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TracePrepareEndData,
) {
	t.failed(ctx, "postgres_prepare_failed", data.Err)
}

// TraceConnectStart implements pgx.ConnectTracer.
func (t *tracer) TraceConnectStart(
	ctx context.Context,
	_ pgx.TraceConnectStartData,
) context.Context {
	return started(ctx)
}

// TraceConnectEnd implements pgx.ConnectTracer.
func (t *tracer) TraceConnectEnd(
	ctx context.Context,
	data pgx.TraceConnectEndData,
) {
	t.connect.Observe(elapsed(ctx))
	t.failed(ctx, "postgres_connect_failed", data.Err)
}

// TraceAcquireStart implements pgxpool.AcquireTracer.
func (t *tracer) TraceAcquireStart(
	ctx context.Context,
	_ *pgxpool.Pool,
	_ pgxpool.TraceAcquireStartData,
) context.Context {
	return started(ctx)
}

// TraceAcquireEnd implements pgxpool.AcquireTracer.
//
// This is the series that does not exist anywhere else and the reason for
// implementing the interface at all. Stat().AcquireDuration is a cumulative
// total, so it yields a mean over all time and no percentiles; how long a
// caller actually waited for a connection is the number that predicts an
// outage.
func (t *tracer) TraceAcquireEnd(
	ctx context.Context,
	_ *pgxpool.Pool,
	data pgxpool.TraceAcquireEndData,
) {
	t.acquire.Observe(elapsed(ctx))
	t.failed(ctx, "postgres_acquire_failed", data.Err)
}

// codeOf reduces an error to a label from a closed vocabulary.
//
// A SQLSTATE is five characters and the set is fixed by the Postgres
// specification, so it is bounded - and it is the most useful label there is:
// 23505 unique violation, 40001 serialization failure, 57014 statement timeout
// and 53300 too many connections are four completely different incidents that
// one error counter cannot tell apart. Everything else folds into one of four
// constants.
func codeOf(err error) string {
	if err == nil {
		return ""
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}

	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}

	var netErr net.Error
	if errors.Is(err, pgconn.ErrConnClosed) || errors.As(err, &netErr) {
		return "conn"
	}
	return "other"
}
