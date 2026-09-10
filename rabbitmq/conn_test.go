package rabbitmq

import (
	"context"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

// run starts the supervisor and returns the Conn, connected. Every test that
// needs a live connection starts this way, and the shutdown it registers is
// the shape a service uses: Close, then wait for Run to return.
func run(t *testing.T, ctx *testcontext.Context, cfg Config) *Conn {
	t.Helper()

	conn := New(testLogger(t), cfg)
	ctx.Go(func() error { return conn.Run(ctx) })

	// Registered first, so it runs last: t.Cleanup is LIFO, and the publishers
	// and consumers a test builds on this connection have to close before it
	// does. The wait is here rather than in a deferred ctx.Cleanup because a
	// deferred one runs before every t.Cleanup - it would wait for a Run that
	// nothing has told to stop yet.
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
		ctx.Wait()
	})

	_, err := conn.Wait(ctx)
	require.NoError(t, err)

	return conn
}

func TestConnConnects(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))

	require.True(t, conn.Connected())
	require.NoError(t, conn.Health(ctx))

	ch, err := conn.Channel(ctx)
	require.NoError(t, err)
	require.NoError(t, ch.Close())
}

// The behavior the whole package exists for: the broker goes away, and the
// service neither dies nor needs restarting.
func TestConnReconnects(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	server := startBroker(t)
	conn := run(t, ctx, server.config(t))

	sessions := make(chan int64, 8)
	ctx.Go(func() error {
		return conn.Session(ctx, func(ctx context.Context, ch *amqp.Channel) error {
			sessions <- generationOf(conn)
			<-ctx.Done()
			return ctx.Err()
		})
	})

	require.Equal(t, int64(1), <-sessions)

	server.stop(t)
	require.Eventually(t, func() bool { return !conn.Connected() }, time.Minute, 50*time.Millisecond)
	require.Error(t, conn.Health(ctx))

	server.start(t)

	// The session function runs again on the new connection, which is what
	// makes topology re-declaration and a re-registered consumer possible at
	// all.
	require.Equal(t, int64(2), <-sessions)
	require.NoError(t, conn.Health(ctx))
}

// Close while the supervisor is mid-backoff has to return promptly, not after
// the backoff it happened to be sleeping through.
func TestCloseDuringBackoffReturnsPromptly(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	cfg := testConfig()
	cfg.Address = "127.0.0.1:1" // nothing is listening
	cfg.ReconnectDelay = 30 * time.Second
	cfg.ReconnectMaxDelay = 30 * time.Second
	cfg.DialTimeout = time.Second

	conn := New(testLogger(t), cfg)

	done := make(chan error, 1)
	go func() { done <- conn.Run(ctx) }()

	// Let it fail once and settle into the backoff.
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, conn.Close())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

// RequireOnStart is the difference between a service that waits for its broker
// and one that refuses to start without it. Both have to be available, and the
// default is to wait.
func TestRequireOnStart(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	cfg := testConfig()
	cfg.Address = "127.0.0.1:1"
	cfg.DialTimeout = time.Second
	cfg.ReconnectDelay = 50 * time.Millisecond
	cfg.RequireOnStart = true

	conn := New(testLogger(t), cfg)
	defer func() { require.NoError(t, conn.Close()) }()

	require.Error(t, conn.Run(ctx))
}

func TestWaitReturnsWhenTheCallerIsDone(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Address = "127.0.0.1:1"

	conn := New(testLogger(t), cfg)
	defer func() { require.NoError(t, conn.Close()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := conn.Wait(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitReturnsWhenTheConnIsClosed(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	cfg := testConfig()
	cfg.Address = "127.0.0.1:1"

	conn := New(testLogger(t), cfg)
	require.NoError(t, conn.Close())

	_, err := conn.Wait(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed")
}

// A fn that fails for its own reasons must not be retried forever: that turns
// a bug into a loop nobody can see the start of.
func TestSessionReturnsACallerError(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))

	sentinel := errors.New("the caller's own problem")
	calls := 0

	err := conn.Session(ctx, func(context.Context, *amqp.Channel) error {
		calls++
		return sentinel
	})

	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, calls)
}

func TestStats(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))

	fields := map[string]float64{}
	conn.Stats(func(key monkit.SeriesKey, field string, val float64) {
		require.Equal(t, "rabbitmq_conn", key.Measurement)
		require.Equal(t, t.Name(), key.Tags.Get("name"))
		fields[field] = val
	})

	require.Equal(t, float64(1), fields["connected"])
	require.Equal(t, float64(1), fields["generation"])
}

func generationOf(conn *Conn) int64 {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return conn.gen
}
