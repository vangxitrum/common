package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/logger/logtest"
	"gitlab.internal/tuan.quang.tran/aioz-common/postgres"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

// These drive a real server, because the point of the query log is what a real
// pgx round trip produces: the statement pgx actually sent, the command tag it
// actually returned, and a duration that was actually measured.

func TestQueryLogAgainstARealServer(t *testing.T) {
	ctx := testcontext.New(t)

	config := containerConfig(t)
	config.LogQueries = true

	log, rec := logtest.NewRecorder()
	db, err := postgres.Open(ctx, log, config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var id int
	require.NoError(t, db.Pool.
		QueryRow(ctx, "SELECT $1::int", 4242).Scan(&id))
	require.Equal(t, 4242, id)

	records := rec.FilterMessage("query")
	require.NotEmpty(t, records, "every statement is logged at debug")

	var found bool
	for _, r := range records {
		if r.Attrs["statement"] == "SELECT $1::int" {
			found = true
			require.Equal(t, "db", r.Attrs["group"])
			require.Equal(t, "SELECT", r.Attrs["operation"])
			require.Positive(t, r.Attrs["duration"])
		}
	}
	require.True(t, found, "the statement pgx sent is the one logged")

	require.NotContains(t, rendered(rec), "4242",
		"the bind parameter never reaches a log line")
}

func TestSlowQueryAgainstARealServer(t *testing.T) {
	ctx := testcontext.New(t)

	config := containerConfig(t)
	config.SlowQuery = 50 * time.Millisecond

	log, rec := logtest.NewRecorder()
	db, err := postgres.Open(ctx, log, config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	// Fast: not logged.
	var one int
	require.NoError(t, db.Pool.QueryRow(ctx, "SELECT 1").Scan(&one))

	// Slow: logged, once, as a warning.
	_, err = db.Exec(ctx, "SELECT pg_sleep(0.2)")
	require.NoError(t, err)

	slow := rec.FilterMessage("slow query")
	require.Len(t, slow, 1, "only the statement over the threshold")
	require.Equal(t, "SELECT pg_sleep(0.2)", slow[0].Attrs["statement"])
	require.Equal(t, 50*time.Millisecond, slow[0].Attrs["threshold"])

	require.Empty(t, rec.FilterMessage("query"),
		"LogQueries is off, so nothing else is logged")
}

func TestAFailedQueryAgainstARealServer(t *testing.T) {
	ctx := testcontext.New(t)

	config := containerConfig(t)
	config.SlowQuery = time.Hour // nothing is slow enough

	log, rec := logtest.NewRecorder()
	db, err := postgres.Open(ctx, log, config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.Exec(ctx, "SELECT * FROM no_such_table")
	require.Error(t, err)

	failures := rec.FilterMessage("query failed")
	require.Len(t, failures, 1,
		"a failure is logged whatever the threshold says")
	require.Equal(t, "SELECT * FROM no_such_table",
		failures[0].Attrs["statement"])
	require.NotNil(t, failures[0].Attrs["err"])
}

func TestQueryLogIsSilentByDefaultAgainstARealServer(t *testing.T) {
	ctx := testcontext.New(t)

	log, rec := logtest.NewRecorder()
	db, err := postgres.Open(ctx, log, containerConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var one int
	require.NoError(t, db.Pool.QueryRow(ctx, "SELECT 1").Scan(&one))

	require.Empty(t, rec.FilterMessage("query"))
	require.Empty(t, rec.FilterMessage("slow query"))
}

func rendered(rec *logtest.Recorder) string { return fmt.Sprint(rec.All()) }
