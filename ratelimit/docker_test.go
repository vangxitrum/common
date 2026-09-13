package ratelimit_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"gitlab.internal/tuan.quang.tran/aioz-common/ratelimit"
	"gitlab.internal/tuan.quang.tran/aioz-common/redis"
)

// requireDocker skips when there is no reachable docker daemon, unless
// AIOZ_REQUIRE_DOCKER is set, which turns the skip into a failure. Copied from
// redis/docker_test.go, including why there is no build tag.
func requireDocker(t *testing.T) {
	t.Helper()

	provider, err := testcontainers.NewDockerProvider()
	if err == nil {
		defer func() { _ = provider.Close() }()
		err = provider.Health(context.Background())
	}
	if err == nil {
		err = configuredDaemon(provider)
	}
	if err == nil {
		return
	}

	if os.Getenv("AIOZ_REQUIRE_DOCKER") != "" {
		t.Fatalf(
			"AIOZ_REQUIRE_DOCKER is set but no docker daemon is reachable: %v",
			err,
		)
	}
	t.Skipf(
		"no docker daemon; set AIOZ_REQUIRE_DOCKER to make this a failure: %v",
		err,
	)
}

// configuredDaemon fails when DOCKER_HOST names a daemon other than the one
// testcontainers ended up on (see redis/docker_test.go).
func configuredDaemon(provider *testcontainers.DockerProvider) error {
	want := os.Getenv("DOCKER_HOST")
	if got := provider.Client().DaemonHost(); want != "" && got != want {
		return fmt.Errorf(
			"DOCKER_HOST is %s, but it did not answer and testcontainers fell back to %s",
			want,
			got,
		)
	}
	return nil
}

// sharedRedis is one server for the whole package; tests keep apart by
// prefix. Ryuk removes it when the test binary exits.
var sharedRedis = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := testcontainers.GenericContainer(
		ctx,
		testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "redis:8-alpine",
				ExposedPorts: []string{"6379/tcp"},
				WaitingFor:   wait.ForListeningPort("6379/tcp"),
			},
			Started: true,
		},
	)
	if err != nil {
		return "", err
	}
	return container.Endpoint(ctx, "")
})

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	requireDocker(t)

	address, err := sharedRedis()
	require.NoError(t, err)

	client, err := redis.Open(t.Context(), quietLogger(), redis.Config{
		Name:           t.Name(),
		Address:        address,
		RequireOnStart: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func redisLimiter(
	t *testing.T,
	client *redis.Client,
	prefix string,
) *ratelimit.Limiter {
	t.Helper()
	l, err := ratelimit.New(quietLogger(), ratelimit.Config{
		Prefix:  prefix,
		OnError: ratelimit.OnErrorClosed, // a Redis problem must fail the test, not fall back
		Timeout: 5 * time.Second,
	}, ratelimit.WithRedis(client))
	require.NoError(t, err)
	return l
}

func TestRedisStore(t *testing.T) {
	t.Parallel()
	runSuite(t, func(t *testing.T) *ratelimit.Limiter {
		return redisLimiter(t, redisClient(t), uniquePrefix())
	})
}

// Two replicas on one Redis share one budget, which is the point of Redis.
func TestLimitsHoldAcrossReplicas(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	client := redisClient(t)
	prefix := uniquePrefix()
	one, two := redisLimiter(t, client, prefix), redisLimiter(t, client, prefix)
	key := rule("login", per(2, time.Hour)).Key("alice@example.com")

	require.True(t, allow(t, ctx, one, key).Allowed)
	require.True(t, allow(t, ctx, two, key).Allowed)
	require.False(t, allow(t, ctx, one, key).Allowed)
	require.False(t, allow(t, ctx, two, key).Allowed)
}

// The script is one atomic step per call, so racing clients never share out
// more than Count.
func TestConcurrentRequestsAcrossClientsNeverExceedTheCount(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	prefix := uniquePrefix()
	limiters := []*ratelimit.Limiter{
		redisLimiter(t, redisClient(t), prefix),
		redisLimiter(t, redisClient(t), prefix),
	}
	key := rule("login", per(50, time.Hour)).Key("alice@example.com")

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range 32 {
		l := limiters[i%2]
		wg.Go(func() {
			for range 5 {
				result, err := l.Allow(ctx, key)
				if err == nil && result.Allowed {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	require.Equal(t, int64(50), allowed.Load())
}

// A key is a digest, so an identifier - an email, an IP - never sits in Redis,
// and a huge one cannot make a huge key.
func TestKeysCarryNoRawValueAndAreBounded(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	client := redisClient(t)
	prefix := uniquePrefix()
	l := redisLimiter(t, client, prefix)

	email := strings.Repeat("a", 10000) + "@example.com"
	allow(
		t,
		ctx,
		l,
		rule("login", per(5, time.Minute), per(20, time.Hour)).Key(email),
	)

	keys, err := client.Keys(ctx, prefix+":*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 2, "one key per rate")
	for _, k := range keys {
		require.NotContains(t, k, "example.com")
		require.LessOrEqual(t, len(k), 128, "%q", k)
	}
}

// A key lives exactly as long as it takes to refill, so Redis holds nothing
// for a user who stopped trying.
func TestKeysExpireWhenFullyRefilled(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	client := redisClient(t)
	prefix := uniquePrefix()
	l := redisLimiter(t, client, prefix)

	allow(
		t,
		ctx,
		l,
		rule("login", per(5, time.Minute)).Key("alice@example.com"),
	)

	keys, err := client.Keys(ctx, prefix+":*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1)

	ttl, err := client.PTTL(ctx, keys[0]).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 11*time.Second)
	require.LessOrEqual(
		t,
		ttl,
		12*time.Second,
		"one event refills in Period/Count",
	)
}

// EVALSHA falls back to EVAL when the server has lost the script, as after a
// restart or a SCRIPT FLUSH.
func TestTheScriptSurvivesAScriptFlush(t *testing.T) {
	t.Parallel()

	ctx, _ := frozenClock(t)
	client := redisClient(t)
	l := redisLimiter(t, client, uniquePrefix())
	key := rule("login", per(5, time.Minute)).Key("alice@example.com")

	require.True(t, allow(t, ctx, l, key).Allowed)
	require.NoError(t, client.ScriptFlush(ctx).Err())
	require.True(t, allow(t, ctx, l, key).Allowed)
}
