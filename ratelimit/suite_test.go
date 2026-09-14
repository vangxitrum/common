package ratelimit_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/ratelimit"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

var prefixes atomic.Int64

// uniquePrefix keeps tests that share one Redis from sharing buckets.
func uniquePrefix() string {
	return fmt.Sprintf("t%d", prefixes.Add(1))
}

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func memoryLimiter(t *testing.T) *ratelimit.Limiter {
	t.Helper()
	l, err := ratelimit.New(quietLogger(), ratelimit.Config{
		Prefix:   uniquePrefix(),
		OnError:  ratelimit.OnErrorLocal,
		Timeout:  time.Second,
		Capacity: 1000,
	})
	require.NoError(t, err)
	return l
}

// frozenClock is a context whose time moves only when the test says so.
func frozenClock(t *testing.T) (context.Context, *time2.Machine) {
	t.Helper()
	return time2.WithNewMachine(t.Context())
}

func rule(name string, rates ...ratelimit.Rate) ratelimit.Rule {
	return ratelimit.Rule{Name: name, Rates: rates}
}

func per(count int, period time.Duration) ratelimit.Rate {
	return ratelimit.Rate{Count: count, Period: period}
}

func allow(
	t *testing.T,
	ctx context.Context,
	l *ratelimit.Limiter,
	keys ...ratelimit.Key,
) ratelimit.Result {
	t.Helper()
	result, err := l.Allow(ctx, keys...)
	require.NoError(t, err)
	return result
}

// runSuite is the behavior both stores must have; the memory store runs it in
// this file, the Redis store in docker_test.go.
func runSuite(t *testing.T, newLimiter func(t *testing.T) *ratelimit.Limiter) {
	t.Run("a burst up to the count is allowed", func(t *testing.T) {
		t.Parallel()
		ctx, _ := frozenClock(t)
		l := newLimiter(t)
		key := rule("login", per(5, time.Minute)).Key("alice@example.com")

		for i := range 5 {
			require.True(t, allow(t, ctx, l, key).Allowed, "request %d", i+1)
		}
		require.False(t, allow(t, ctx, l, key).Allowed, "the sixth")
	})

	// 5 per minute is one every 12s once the burst is spent.
	t.Run(
		"the next request is denied with the exact retry-after",
		func(t *testing.T) {
			t.Parallel()
			ctx, _ := frozenClock(t)
			l := newLimiter(t)
			key := rule("login", per(5, time.Minute)).Key("alice@example.com")

			for range 5 {
				allow(t, ctx, l, key)
			}
			result := allow(t, ctx, l, key)
			require.False(t, result.Allowed)
			require.Equal(t, 12*time.Second, result.RetryAfter)
		},
	)

	t.Run("advancing by the retry-after allows again", func(t *testing.T) {
		t.Parallel()
		ctx, machine := frozenClock(t)
		l := newLimiter(t)
		key := rule("login", per(5, time.Minute)).Key("alice@example.com")

		for range 5 {
			allow(t, ctx, l, key)
		}
		denied := allow(t, ctx, l, key)
		require.False(t, denied.Allowed)

		machine.Advance(denied.RetryAfter - time.Millisecond)
		require.False(t, allow(t, ctx, l, key).Allowed, "a millisecond early")

		machine.Advance(time.Millisecond)
		require.True(t, allow(t, ctx, l, key).Allowed)
	})

	// The property that makes this a delay rather than a lockout: an attacker
	// hammering a victim's key cannot push the victim's refill further out.
	t.Run("denied requests consume nothing", func(t *testing.T) {
		t.Parallel()
		ctx, machine := frozenClock(t)
		l := newLimiter(t)
		key := rule("login", per(5, time.Minute)).Key("alice@example.com")

		for range 5 {
			allow(t, ctx, l, key)
		}
		for range 1000 {
			require.False(t, allow(t, ctx, l, key).Allowed)
		}
		machine.Advance(12 * time.Second)
		require.True(t, allow(t, ctx, l, key).Allowed)
	})

	// 5 per minute and 20 per hour, for a client that retries the moment it
	// is told to: over 10 minutes the long window allows its burst of 20 and
	// then one every 3 minutes, 23 in all. The short window alone would allow
	// 5 + 600/12 = 55.
	t.Run(
		"the long window holds after the short one refills",
		func(t *testing.T) {
			t.Parallel()
			ctx, machine := frozenClock(t)
			l := newLimiter(t)
			key := rule(
				"login",
				per(5, time.Minute),
				per(20, time.Hour),
			).Key("alice@example.com")

			deadline := machine.Now().Add(10 * time.Minute)
			allowed := 0
			for {
				result := allow(t, ctx, l, key)
				if result.Allowed {
					allowed++
					continue
				}
				if machine.Now().Add(result.RetryAfter).After(deadline) {
					break
				}
				machine.Advance(result.RetryAfter)
			}

			require.Equal(t, 23, allowed)
		},
	)

	// Several keys in one call: allowed only if every key allows, and a denial
	// in one consumes nothing in the others.
	t.Run(
		"a denial in one key consumes none of the others",
		func(t *testing.T) {
			t.Parallel()
			ctx, _ := frozenClock(t)
			l := newLimiter(t)
			strict := rule(
				"login-account",
				per(1, time.Hour),
			).Key("alice@example.com")
			loose := rule("login-ip", per(5, time.Minute)).Key("203.0.113.9")

			require.True(t, allow(t, ctx, l, strict).Allowed)
			for range 10 {
				require.False(t, allow(t, ctx, l, strict, loose).Allowed)
			}
			for i := range 5 {
				require.True(
					t,
					allow(t, ctx, l, loose).Allowed,
					"loose request %d",
					i+1,
				)
			}
		},
	)

	t.Run("keys are independent", func(t *testing.T) {
		t.Parallel()
		ctx, _ := frozenClock(t)
		l := newLimiter(t)
		login := rule("login", per(1, time.Hour))

		require.True(
			t,
			allow(t, ctx, l, login.Key("alice@example.com")).Allowed,
		)
		require.False(
			t,
			allow(t, ctx, l, login.Key("alice@example.com")).Allowed,
		)
		require.True(t, allow(t, ctx, l, login.Key("bob@example.com")).Allowed)
	})

	t.Run("reset forgives only the given key", func(t *testing.T) {
		t.Parallel()
		ctx, _ := frozenClock(t)
		l := newLimiter(t)
		login := rule("login", per(1, time.Hour))
		alice, bob := login.Key(
			"alice@example.com",
		), login.Key(
			"bob@example.com",
		)

		allow(t, ctx, l, alice)
		allow(t, ctx, l, bob)
		require.NoError(t, l.Reset(ctx, alice))

		require.True(t, allow(t, ctx, l, alice).Allowed)
		require.False(t, allow(t, ctx, l, bob).Allowed)
	})
}

func TestMemoryStore(t *testing.T) {
	t.Parallel()
	runSuite(t, memoryLimiter)
}
