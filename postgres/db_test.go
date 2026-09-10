package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/postgres"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

func containerConfig(t *testing.T) postgres.Config {
	t.Helper()

	host, port := startPostgres(t)

	config := baseConfig(t)
	config.Host = host
	config.Port = port
	config.Database = testDatabase
	config.User = testUser
	config.Password = testPassword
	config.SSLMode = "disable" // the container has no certificate
	return config
}

func open(
	t *testing.T,
	ctx context.Context,
	config postgres.Config,
) *postgres.DB {
	t.Helper()

	db, err := postgres.Open(ctx, testLogger(t), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// The embedded pool is the API: this exercises it unwrapped, and pins the one
// thing callers get wrong - an empty result is not a failure.
func TestOpenPingsTheServerAndQueriesWork(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	db := open(t, ctx, containerConfig(t))
	require.NoError(t, db.Health(ctx))

	_, err := db.Exec(
		ctx,
		`CREATE TABLE greetings (id int primary key, body text not null)`,
	)
	require.NoError(t, err)

	_, err = db.Exec(
		ctx,
		`INSERT INTO greetings (id, body) VALUES ($1, $2)`,
		1,
		"hello",
	)
	require.NoError(t, err)

	var body string
	require.NoError(
		t,
		db.QueryRow(ctx, `SELECT body FROM greetings WHERE id = $1`, 1).
			Scan(&body),
	)
	require.Equal(t, "hello", body)

	err = db.QueryRow(ctx, `SELECT body FROM greetings WHERE id = $1`, 404).
		Scan(&body)
	require.ErrorIs(
		t,
		err,
		pgx.ErrNoRows,
		"an empty lookup is ErrNoRows, not a failure",
	)
}

func TestOpenFailsOnBadCredentials(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	for name, mutate := range map[string]func(*postgres.Config){
		"a wrong password":    func(c *postgres.Config) { c.Password = "not-" + testPassword },
		"a missing database":  func(c *postgres.Config) { c.Database = "nowhere" },
		"an unknown username": func(c *postgres.Config) { c.User = "nobody" },
	} {
		t.Run(name, func(t *testing.T) {
			config := containerConfig(t)
			mutate(&config)

			_, err := postgres.Open(ctx, testLogger(t), config)
			require.Error(t, err)
			require.NotContains(t, err.Error(), config.Password,
				"a logged credential is a leaked credential")
		})
	}
}

// The assertion that proves the settings are on the pool rather than on one
// connection: two concurrent connections must see the same values.
func TestSessionSettingsReachEveryConnection(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := containerConfig(t)
	config.ApplicationName = "the-service"
	config.SearchPath = "public"
	config.StatementTimeout = 9 * time.Second
	config.MinConns = 2
	config.MaxConns = 4

	db := open(t, ctx, config)

	const query = `SELECT current_setting('application_name'),
	                      current_setting('search_path'),
	                      current_setting('statement_timeout')`

	read := func() [3]string {
		var out [3]string
		conn, err := db.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()

		require.NoError(
			t,
			conn.QueryRow(ctx, query).Scan(&out[0], &out[1], &out[2]),
		)
		return out
	}

	// Held at the same time, so these are two different connections.
	first, err := db.Acquire(ctx)
	require.NoError(t, err)

	second := read()
	first.Release()

	firstValues := read()

	require.Equal(t, [3]string{"the-service", "public", "9s"}, firstValues)
	require.Equal(
		t,
		firstValues,
		second,
		"every connection in the pool must be identical",
	)
}

func TestStatementTimeoutCancelsALongQuery(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := containerConfig(t)
	config.StatementTimeout = 100 * time.Millisecond

	db := open(t, ctx, config)

	_, err := db.Exec(ctx, `SELECT pg_sleep(5)`)
	require.Error(t, err)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(
		t,
		"57014",
		pgErr.Code,
		"query_canceled is what a statement timeout looks like",
	)
}

func TestStats(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := containerConfig(t)
	db := open(t, ctx, config)

	// The pool is empty until first use - MinConns defaults to zero - so the
	// query has to happen before there is anything to report.
	var one int
	require.NoError(t, db.QueryRow(ctx, `SELECT 1`).Scan(&one))

	fields := map[string]float64{}
	db.Stats(func(key monkit.SeriesKey, field string, value float64) {
		if key.Measurement == "postgres_pool" &&
			key.Tags.Get("name") == config.Name {
			fields[field] = value
		}
	})

	for _, field := range []string{
		"acquire_count", "acquire_duration_seconds", "acquired_conns",
		"canceled_acquire_count", "constructing_conns", "empty_acquire_count",
		"idle_conns", "max_conns", "total_conns", "new_conns_count",
		"max_lifetime_destroy_count", "max_idle_destroy_count",
		"empty_acquire_wait_seconds",
	} {
		require.Contains(t, fields, field)
	}
	require.Positive(t, fields["total_conns"])
	require.Positive(t, fields["acquire_count"])
}

// The only test that proves the AcquireTracer assertion actually binds: pgxpool
// finds it by a runtime type assertion, so a drifted signature would silently
// record nothing.
func TestAcquireWaitIsMeasured(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := containerConfig(t)
	config.MaxConns = 1

	db := open(t, ctx, config)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out int
			_ = db.QueryRow(ctx, `SELECT pg_sleep(0.05), 1`).
				Scan(new(any), &out)
		}()
	}
	wg.Wait()

	require.Positive(
		t,
		seriesValue(t, "postgres_acquire", config.Name, "count"),
		"acquire timings must be recorded",
	)

	var empties float64
	db.Stats(func(key monkit.SeriesKey, field string, value float64) {
		if key.Measurement == "postgres_pool" &&
			field == "empty_acquire_count" &&
			key.Tags.Get("name") == config.Name {
			empties = value
		}
	})
	require.Positive(
		t,
		empties,
		"four callers through one connection must have waited",
	)
}

// pgx synthesizes ErrNoRows after the trace has already ended, so it cannot
// reach the metrics at all. This pins the outcome that matters either way.
func TestErrNoRowsIsNotCountedAsAFailure(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := containerConfig(t)
	db := open(t, ctx, config)

	before := seriesValue(t, "postgres_query", config.Name, "count")

	for range 3 {
		var out int
		err := db.QueryRow(ctx, `SELECT 1 WHERE false`).Scan(&out)
		require.ErrorIs(t, err, pgx.ErrNoRows)
	}

	require.Equal(
		t,
		before+3,
		seriesValue(t, "postgres_query", config.Name, "count"),
		"the queries were timed",
	)

	for _, key := range publishedKeys(t, config.Name) {
		require.NotEqual(t, "postgres_query_failed", key.Measurement,
			"an empty result is not an error")
	}
}

// The regression test for the cardinality rule. Without it, someone adds an
// operation tag to the histogram and nobody notices until the metrics backend
// does.
func TestTheQueryHistogramIsTaggedOnlyByPool(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := containerConfig(t)
	db := open(t, ctx, config)

	var out int
	require.NoError(
		t,
		db.QueryRow(postgres.WithOperation(ctx, "read_one"), `SELECT 1`).
			Scan(&out),
	)

	var checked bool
	for _, key := range publishedKeys(t, config.Name) {
		if key.Measurement != "postgres_query" {
			continue
		}
		checked = true
		require.NotContains(
			t,
			key.Tags.All(),
			"op",
			"an operation tag belongs on the failure event, never on the histogram",
		)
		require.NotContains(t, key.Tags.All(), "sql")
	}
	require.True(t, checked)
}

func TestOperationTagsTheFailure(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := containerConfig(t)
	db := open(t, ctx, config)

	_, err := db.Exec(
		postgres.WithOperation(ctx, "broken_statement"),
		`SELECT nonsense_column`,
	)
	require.Error(t, err)

	var found bool
	for _, key := range publishedKeys(t, config.Name) {
		if key.Measurement == "postgres_query_failed" &&
			key.Tags.Get("op") == "broken_statement" &&
			key.Tags.Get("code") == "42703" { // undefined_column
			found = true
		}
	}
	require.True(
		t,
		found,
		"the failure must name both the operation and the SQLSTATE",
	)
}

func TestTwoPoolsOnOneServer(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	shared := containerConfig(t)

	first := shared
	first.Name = shared.Name + "-first"
	second := shared
	second.Name = shared.Name + "-second"

	firstDB := open(t, ctx, first)
	secondDB := open(t, ctx, second)

	var out int
	require.NoError(t, firstDB.QueryRow(ctx, `SELECT 1`).Scan(&out))
	require.NoError(t, secondDB.QueryRow(ctx, `SELECT 2`).Scan(&out))

	require.Positive(t, seriesValue(t, "postgres_query", first.Name, "count"))
	require.Positive(t, seriesValue(t, "postgres_query", second.Name, "count"))
}

func seriesValue(t *testing.T, measurement, name, field string) float64 {
	t.Helper()

	var found float64
	monkit.Default.Stats(func(key monkit.SeriesKey, f string, value float64) {
		if key.Measurement == measurement && key.Tags.Get("name") == name &&
			f == field {
			found = value
		}
	})
	return found
}

func publishedKeys(t *testing.T, name string) []monkit.SeriesKey {
	t.Helper()

	var keys []monkit.SeriesKey
	monkit.Default.Stats(func(key monkit.SeriesKey, _ string, _ float64) {
		if key.Tags.Get("name") == name {
			keys = append(keys, key)
		}
	})
	return keys
}
