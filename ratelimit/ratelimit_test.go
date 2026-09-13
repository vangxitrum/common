package ratelimit_test

import (
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/config"
	"gitlab.internal/tuan.quang.tran/aioz-common/ratelimit"
)

// Bind must accept the tags, and a service's own Rates field must take its
// default through pflag.Value.
func TestConfigBinds(t *testing.T) {
	t.Parallel()

	var cfg struct {
		RateLimit ratelimit.Config
		Login     ratelimit.Rates `help:"login limits" default:"5/1m,20/1h"`
	}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NotPanics(t, func() { config.BindFlags(flags, &cfg) })

	require.Equal(t, "ratelimit", cfg.RateLimit.Prefix)
	require.Equal(t, ratelimit.OnErrorLocal, cfg.RateLimit.OnError)
	require.Equal(t, 200*time.Millisecond, cfg.RateLimit.Timeout)
	require.Equal(t, 100000, cfg.RateLimit.Capacity)
	require.Equal(
		t,
		ratelimit.Rates{per(5, time.Minute), per(20, time.Hour)},
		cfg.Login,
	)
}

func TestRatesParseAndRoundTrip(t *testing.T) {
	t.Parallel()

	var rates ratelimit.Rates
	require.NoError(t, rates.Set("5/1m, 20/1h"))
	require.Equal(
		t,
		ratelimit.Rates{per(5, time.Minute), per(20, time.Hour)},
		rates,
	)

	var again ratelimit.Rates
	require.NoError(t, again.Set(rates.String()))
	require.Equal(t, rates, again, "String parses back to the same rates")

	var empty ratelimit.Rates
	require.NoError(t, empty.Set(""), "config.Bind sets an empty default")
	require.Empty(t, empty)
	require.Equal(t, "rates", empty.Type())
}

func TestRatesRefuse(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"5",          // no period
		"0/1m",       // no requests
		"-1/1m",      // negative
		"5/0s",       // no time
		"5/-1m",      // negative time
		"x/1m",       // not a number
		"5/soon",     // not a duration
		"2000000/1s", // one per half microsecond: finer than the clock
	} {
		var rates ratelimit.Rates
		require.Error(t, rates.Set(input), "%q", input)
	}
}

func TestNewRejects(t *testing.T) {
	t.Parallel()

	for name, config := range map[string]ratelimit.Config{
		"an unknown failure mode":    {OnError: "sometimes"},
		"a prefix with a hash tag":   {Prefix: "a{b}"},
		"a prefix with a space":      {Prefix: "rate limit"},
		"an overlong prefix":         {Prefix: strings.Repeat("p", 33)},
		"a negative timeout":         {Timeout: -time.Second},
		"a negative memory capacity": {Capacity: -1},
	} {
		_, err := ratelimit.New(quietLogger(), config)
		require.Error(t, err, name)
	}
}

// The zero Config is the defaults: a config built by hand, rather than bound
// from flags, still gets a timeout and a bounded memory store.
func TestTheZeroConfigWorks(t *testing.T) {
	t.Parallel()

	l, err := ratelimit.New(quietLogger(), ratelimit.Config{})
	require.NoError(t, err)

	ctx, _ := frozenClock(t)
	require.True(
		t,
		allow(
			t,
			ctx,
			l,
			rule("login", per(1, time.Hour)).Key("alice@example.com"),
		).Allowed,
	)
}

// An empty identifier would put everyone without one into a single shared
// bucket - one attacker then denies them all - so it is an error, not a key.
func TestAnEmptyOrInvalidKeyIsAnError(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := memoryLimiter(t)
	login := rule("login", per(5, time.Minute))

	for name, key := range map[string]ratelimit.Key{
		"an empty value":            login.Key(""),
		"an invalid address":        login.IPKey(netip.Addr{}),
		"a rule with no rates":      rule("login").Key("alice@example.com"),
		"a rule named with caps":    rule("Login", per(5, time.Minute)).Key("alice@example.com"),
		"a rule named with a space": rule("login ip", per(5, time.Minute)).Key("alice@example.com"),
		"an unnamed rule":           rule("", per(5, time.Minute)).Key("alice@example.com"),
		"the zero key":              {},
	} {
		_, err := l.Allow(ctx, key)
		require.Error(t, err, name)
	}

	_, err := l.Allow(ctx)
	require.Error(t, err, "no keys at all")
}

// One subscriber usually gets a whole IPv6 /64, so keying single addresses
// would hand an attacker 2^64 fresh buckets.
func TestAddressesInOneIPv6Slash64ShareABucket(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := memoryLimiter(t)
	byIP := rule("login-ip", per(1, time.Hour))

	require.True(
		t,
		allow(
			t,
			ctx,
			l,
			byIP.IPKey(netip.MustParseAddr("2001:db8::1")),
		).Allowed,
	)
	require.False(
		t,
		allow(
			t,
			ctx,
			l,
			byIP.IPKey(netip.MustParseAddr("2001:db8::ffff")),
		).Allowed,
		"same /64",
	)
	require.True(
		t,
		allow(
			t,
			ctx,
			l,
			byIP.IPKey(netip.MustParseAddr("2001:db8:0:1::1")),
		).Allowed,
		"the next /64",
	)
}

func TestMappedIPv4SharesTheIPv4Bucket(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := memoryLimiter(t)
	byIP := rule("login-ip", per(1, time.Hour))

	require.True(
		t,
		allow(
			t,
			ctx,
			l,
			byIP.IPKey(netip.MustParseAddr("203.0.113.9")),
		).Allowed,
	)
	require.False(
		t,
		allow(
			t,
			ctx,
			l,
			byIP.IPKey(netip.MustParseAddr("::ffff:203.0.113.9")),
		).Allowed,
	)
}

func TestMemoryEvictsTheLeastRecentlyUsedAtCapacity(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l, err := ratelimit.New(quietLogger(), ratelimit.Config{Capacity: 2})
	require.NoError(t, err)
	login := rule("login", per(1, time.Hour))

	allow(t, ctx, l, login.Key("a"))
	allow(t, ctx, l, login.Key("b"))
	allow(t, ctx, l, login.Key("c")) // evicts a, the least recently used

	require.True(t, allow(t, ctx, l, login.Key("a")).Allowed, "a was forgotten")
	require.False(
		t,
		allow(t, ctx, l, login.Key("c")).Allowed,
		"c is still remembered",
	)
}

// Exactly Count requests get through, however many race for them.
func TestMemoryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	l := memoryLimiter(t)
	key := rule("login", per(100, time.Hour)).Key("alice@example.com")

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 10 {
				result, err := l.Allow(ctx, key)
				if err == nil && result.Allowed {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	require.Equal(t, int64(100), allowed.Load())
}

// Rounding down would tell a client "retry in 0 seconds", which is "now".
func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	t.Parallel()

	require.Equal(
		t,
		int64(0),
		ratelimit.Result{Allowed: true}.RetryAfterSeconds(),
	)
	require.Equal(
		t,
		int64(1),
		ratelimit.Result{RetryAfter: time.Millisecond}.RetryAfterSeconds(),
	)
	require.Equal(
		t,
		int64(12),
		ratelimit.Result{RetryAfter: 12 * time.Second}.RetryAfterSeconds(),
	)
	require.Equal(
		t,
		int64(13),
		ratelimit.Result{
			RetryAfter: 12*time.Second + 100*time.Millisecond,
		}.RetryAfterSeconds(),
	)
}

// WithRedis(nil) is a wiring mistake, not a request for the memory store:
// leaving the option out is how a service asks for memory.
func TestWithRedisNeedsAClient(t *testing.T) {
	t.Parallel()

	_, err := ratelimit.New(quietLogger(), ratelimit.Config{}, ratelimit.WithRedis(nil))
	require.Error(t, err)
}
