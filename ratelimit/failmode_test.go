package ratelimit_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/ratelimit"
	"gitlab.internal/tuan.quang.tran/aioz-common/redis"
)

// closedPort returns an address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := ln.Addr().String()
	require.NoError(t, ln.Close())
	return address
}

// hungServer accepts connections and never answers, the way a Redis stuck
// behind a full disk or a GC pause does.
func hungServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// unreachableClient is a pool pointed at address that does not fail at
// construction.
func unreachableClient(t *testing.T, address string) *redis.Client {
	t.Helper()
	client, err := redis.Open(t.Context(), quietLogger(), redis.Config{
		Name:           t.Name(),
		Address:        address,
		MaxRetries:     -1,
		DialTimeout:    100 * time.Millisecond,
		ReadTimeout:    2 * time.Second, // longer than the limiter's bound on purpose
		HealthTimeout:  100 * time.Millisecond,
		RequireOnStart: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func limiterOn(
	t *testing.T,
	client *redis.Client,
	mode string,
	log *slog.Logger,
) *ratelimit.Limiter {
	t.Helper()
	l, err := ratelimit.New(log, ratelimit.Config{
		Prefix:   uniquePrefix(),
		OnError:  mode,
		Timeout:  200 * time.Millisecond,
		Capacity: 100,
	}, ratelimit.WithRedis(client))
	require.NoError(t, err)
	return l
}

// closed: a Redis outage becomes a refusal the caller reports as 503, never a
// silent pass.
func TestOnErrorClosedDenies(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := limiterOn(
		t,
		unreachableClient(t, closedPort(t)),
		ratelimit.OnErrorClosed,
		quietLogger(),
	)

	result, err := l.Allow(
		ctx,
		rule("login", per(5, time.Minute)).Key("alice@example.com"),
	)
	require.Error(t, err)
	require.False(t, result.Allowed)
}

func TestOnErrorOpenAllows(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := limiterOn(
		t,
		unreachableClient(t, closedPort(t)),
		ratelimit.OnErrorOpen,
		quietLogger(),
	)

	result, err := l.Allow(
		ctx,
		rule("login", per(1, time.Hour)).Key("alice@example.com"),
	)
	require.NoError(t, err)
	require.True(t, result.Allowed)
}

// local, the default: protection continues per replica while Redis is down.
func TestOnErrorLocalFallsBackToMemory(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := limiterOn(
		t,
		unreachableClient(t, closedPort(t)),
		ratelimit.OnErrorLocal,
		quietLogger(),
	)
	key := rule("login", per(1, time.Hour)).Key("alice@example.com")

	require.True(t, allow(t, ctx, l, key).Allowed)
	require.False(t, allow(t, ctx, l, key).Allowed, "the fallback still limits")
}

// go-redis does not apply a context deadline to socket reads by default, so a
// server that accepts and never answers would hold Allow for the whole read
// timeout on every retry. The limiter's own Timeout is the bound.
func TestAHungRedisIsBoundedByTheTimeout(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := limiterOn(
		t,
		unreachableClient(t, hungServer(t)),
		ratelimit.OnErrorLocal,
		quietLogger(),
	)

	start := time.Now()
	result, err := l.Allow(
		ctx,
		rule("login", per(1, time.Hour)).Key("alice@example.com"),
	)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.True(t, result.Allowed, "answered from the local fallback")
	require.Less(
		t,
		elapsed,
		time.Second,
		"bounded by the 200ms timeout, not the socket's",
	)
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An outage logs once, not once per login attempt, and never logs a key.
func TestAFailureIsLoggedOnceNotPerRequest(t *testing.T) {
	t.Parallel()

	logs := &logBuffer{}
	log := slog.New(
		slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	ctx, _ := frozenClock(t)
	l := limiterOn(
		t,
		unreachableClient(t, closedPort(t)),
		ratelimit.OnErrorLocal,
		log,
	)

	for range 100 {
		_, _ = l.Allow(
			context.WithoutCancel(ctx),
			rule("login", per(1000, time.Hour)).Key("alice@example.com"),
		)
	}

	require.Equal(
		t,
		1,
		strings.Count(logs.String(), "level=WARN"),
		logs.String(),
	)
	require.NotContains(t, logs.String(), "alice@example.com")
}
