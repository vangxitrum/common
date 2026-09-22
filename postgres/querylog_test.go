package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/logger/logtest"
)

var errTest = errors.New("relation does not exist")

func queryTracer(t *testing.T, cfg Config) (*tracer, *logtest.Recorder) {
	t.Helper()
	log, rec := logtest.NewRecorder()
	return newTracer(log, monkit.NewSeriesTag("name", "test"), cfg), rec
}

// backdate rewinds the tracer's start time so a query appears to have taken
// took, without a test sleeping for it.
func backdate(ctx context.Context, took time.Duration) context.Context {
	return context.WithValue(ctx, startKey{}, time.Now().Add(-took))
}

// recorded renders every record, for a test asserting on what is absent.
func recorded(rec *logtest.Recorder) string { return fmt.Sprint(rec.All()) }

// run drives one query through the tracer the way pgx does.
func run(tr *tracer, sql string, took time.Duration, tag string, err error) {
	ctx := tr.TraceQueryStart(
		context.Background(),
		nil,
		pgx.TraceQueryStartData{
			SQL:  sql,
			Args: []any{"secret@example.com", 42},
		},
	)
	ctx = backdate(ctx, took)
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		CommandTag: pgconn.NewCommandTag(tag),
		Err:        err,
	})
}

func TestQueryLogIsOffByDefault(t *testing.T) {
	tr, rec := queryTracer(t, Config{})

	run(tr, "SELECT 1", time.Second, "SELECT 1", nil)

	require.Empty(t, rec.All(),
		"a pool that asked for neither logs nothing, however slow the query")
}

func TestLogQueriesLogsEveryStatementAtDebug(t *testing.T) {
	tr, rec := queryTracer(t, Config{LogQueries: true})

	run(tr, "SELECT id FROM users WHERE email = $1", 3*time.Millisecond,
		"SELECT 1", nil)

	records := rec.All()
	require.Len(t, records, 1)
	require.Equal(t, "debug", strings.ToLower(records[0].Level.String()))
	require.Equal(t, "db", records[0].Attrs["group"])
	require.Equal(t, "SELECT id FROM users WHERE email = $1",
		records[0].Attrs["statement"])
	require.Equal(t, "SELECT", records[0].Attrs["operation"])
	require.Equal(t, int64(1), records[0].Attrs["rows"])
	require.GreaterOrEqual(t,
		records[0].Attrs["duration"], 3*time.Millisecond,
		"the duration is measured, so it is at least what was backdated")
	require.Equal(t, "test", records[0].Attrs["name"])
}

func TestArgumentsAreNeverLogged(t *testing.T) {
	tr, rec := queryTracer(t, Config{LogQueries: true})

	run(tr, "SELECT id FROM users WHERE email = $1", time.Millisecond,
		"SELECT 1", nil)

	require.NotContains(
		t,
		recorded(rec),
		"secret@example.com",
		"bind parameters are the PII; only the parameterized statement is logged",
	)
	require.NotContains(t, recorded(rec), "42")
}

func TestSlowQueryLogsAtWarnOnly(t *testing.T) {
	tr, rec := queryTracer(t, Config{SlowQuery: 100 * time.Millisecond})

	run(tr, "SELECT pg_sleep(1)", 250*time.Millisecond, "SELECT 1", nil)
	run(tr, "SELECT 1", 5*time.Millisecond, "SELECT 1", nil)

	records := rec.All()
	require.Len(t, records, 1, "only the slow one is logged")
	require.Equal(t, "warn", strings.ToLower(records[0].Level.String()))
	require.Equal(t, "SELECT pg_sleep(1)", records[0].Attrs["statement"])
	require.Equal(t, 100*time.Millisecond, records[0].Attrs["threshold"])
}

func TestSlowQueryWinsOverLogQueries(t *testing.T) {
	// Both on: a slow statement is a warning, not a debug line repeated.
	tr, rec := queryTracer(t, Config{
		LogQueries: true, SlowQuery: 100 * time.Millisecond,
	})

	run(tr, "SELECT pg_sleep(1)", 250*time.Millisecond, "SELECT 1", nil)

	records := rec.All()
	require.Len(t, records, 1)
	require.Equal(t, "warn", strings.ToLower(records[0].Level.String()))
}

func TestAFailedQueryIsLoggedWhateverItsDuration(t *testing.T) {
	tr, rec := queryTracer(t, Config{SlowQuery: time.Hour})

	run(tr, "SELECT nope", time.Millisecond, "", errTest)

	records := rec.All()
	require.Len(t, records, 1)
	require.Equal(t, "error", strings.ToLower(records[0].Level.String()))
	require.Equal(t, "SELECT nope", records[0].Attrs["statement"])
	require.NotNil(t, records[0].Attrs["err"])
}

func TestNoRowsIsNotAFailure(t *testing.T) {
	tr, rec := queryTracer(t, Config{SlowQuery: time.Hour})

	run(tr, "SELECT 1", time.Millisecond, "SELECT 0", pgx.ErrNoRows)

	require.Empty(t, rec.All(),
		"pgx.ErrNoRows is an empty result, not an error")
}

func TestTheOperationIsIncludedWhenSet(t *testing.T) {
	tr, rec := queryTracer(t, Config{LogQueries: true})

	ctx := WithOperation(context.Background(), "invoice reconciliation")
	ctx = tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	ctx = backdate(ctx, time.Millisecond)
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		CommandTag: pgconn.NewCommandTag("SELECT 1"),
	})

	records := rec.All()
	require.Len(t, records, 1)
	require.Equal(t, "invoice reconciliation", records[0].Attrs["op"])
}

func TestALongStatementIsTruncated(t *testing.T) {
	tr, rec := queryTracer(t, Config{LogQueries: true})

	long := "SELECT " + strings.Repeat("x", maxStatementLen*2)
	run(tr, long, time.Millisecond, "SELECT 1", nil)

	records := rec.All()
	require.Len(t, records, 1)
	statement, _ := records[0].Attrs["statement"].(string)
	require.LessOrEqual(t, len(statement), maxStatementLen+len(truncated))
	require.True(t, strings.HasSuffix(statement, truncated),
		"a truncated statement says so, so nobody reads it as the whole query")
}

func TestWhitespaceIsCollapsed(t *testing.T) {
	// A Go raw-string query arrives with newlines and indentation, which turn
	// one log line into twenty.
	tr, rec := queryTracer(t, Config{LogQueries: true})

	run(tr, "SELECT id,\n\t   name\nFROM users\nWHERE id = $1",
		time.Millisecond, "SELECT 1", nil)

	records := rec.All()
	require.Len(t, records, 1)
	require.Equal(t, "SELECT id, name FROM users WHERE id = $1",
		records[0].Attrs["statement"])
}
