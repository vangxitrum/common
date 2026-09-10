package redis_test

import (
	"log/slog"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/redis"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

// testLogger routes the package's log lines into the test output, so a failure
// shows what the client was doing.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p))
	return len(p), nil
}

func testConfig(t *testing.T, address string) redis.Config {
	t.Helper()

	cfg := baseConfig()
	cfg.Address = address
	cfg.Name = t.Name()
	return cfg
}

// The whole reason Open exists: a reachable server is proven at startup rather
// than on the first business request.
func TestOpenPingsTheServer(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	client, err := redis.Open(ctx, testLogger(t), testConfig(t, startRedis(t)))
	require.NoError(t, err)
	defer ctx.Check(client.Close)

	require.NoError(t, client.Health(ctx))

	// The embedded client is the API, so a command needs no wrapper.
	require.NoError(t, client.Set(ctx, "k", "v", time.Minute).Err())

	value, err := client.Get(ctx, "k").Result()
	require.NoError(t, err)
	require.Equal(t, "v", value)

	// And a miss is goredis.Nil, not an error the caller has to decode.
	_, err = client.Get(ctx, "absent").Result()
	require.ErrorIs(t, err, goredis.Nil)
}

func TestOpenFailsOnAWrongPassword(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	const password = "the-password"

	cfg := testConfig(t, startRedisWithPassword(t, password))
	cfg.Password = "not-" + password

	_, err := redis.Open(ctx, testLogger(t), cfg)
	require.Error(t, err)
}

// RequireOnStart is the switch between "this cache is essential" and "this
// cache is an optimisation". Both paths have to work, and the off path must
// still hand back a usable client.
func TestRequireOnStart(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	// A port nothing is listening on. Dialing it fails fast rather than
	// hanging, which is what makes this a unit-speed test.
	const dead = "127.0.0.1:1"

	deadConfig := func(t *testing.T) redis.Config {
		cfg := testConfig(t, dead)
		// go-redis retries a failed dial per command, so the defaults would
		// turn one unreachable address into fifteen seconds of test.
		cfg.MaxRetries = -1
		cfg.DialTimeout = 500 * time.Millisecond
		cfg.HealthTimeout = 2 * time.Second
		return cfg
	}

	t.Run("on", func(t *testing.T) {
		cfg := deadConfig(t)
		cfg.RequireOnStart = true

		_, err := redis.Open(ctx, testLogger(t), cfg)
		require.Error(t, err)
	})

	t.Run("off", func(t *testing.T) {
		cfg := deadConfig(t)
		cfg.RequireOnStart = false

		client, err := redis.Open(ctx, testLogger(t), cfg)
		require.NoError(t, err)
		require.NotNil(t, client)
		defer ctx.Check(client.Close)

		// Reachability is still reported honestly on demand.
		require.Error(t, client.Health(ctx))
	})
}

// aioz-stream runs two pools against one server on different logical
// databases. That is two Open calls with different Name and DB, and the
// distinct Name is what keeps their metrics apart.
func TestTwoDatabasesOnOneServer(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	address := startRedis(t)
	log := testLogger(t)

	first := testConfig(t, address)
	first.Name = "first"
	first.DB = 0

	second := testConfig(t, address)
	second.Name = "second"
	second.DB = 1

	one, err := redis.Open(ctx, log, first)
	require.NoError(t, err)
	defer ctx.Check(one.Close)

	two, err := redis.Open(ctx, log, second)
	require.NoError(t, err)
	defer ctx.Check(two.Close)

	require.NoError(t, one.Set(ctx, "shared-key", "one", time.Minute).Err())

	_, err = two.Get(ctx, "shared-key").Result()
	require.ErrorIs(t, err, goredis.Nil)
}

// Stats has to survive being chained, because that is the only way a service
// ever calls it.
func TestStats(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	client, err := redis.Open(ctx, testLogger(t), testConfig(t, startRedis(t)))
	require.NoError(t, err)
	defer ctx.Check(client.Close)

	require.NoError(t, client.Set(ctx, "k", "v", time.Minute).Err())

	fields := map[string]float64{}
	client.Stats(func(key monkit.SeriesKey, field string, val float64) {
		require.Equal(t, "redis_pool", key.Measurement)
		require.Equal(t, t.Name(), key.Tags.Get("name"))
		fields[field] = val
	})

	for _, field := range []string{"hits", "misses", "timeouts", "total_conns", "idle_conns", "stale_conns"} {
		require.Contains(t, fields, field)
	}
	require.Positive(t, fields["total_conns"])
}
