package redis_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// requireDocker skips when there is no reachable docker daemon, unless
// AIOZ_REQUIRE_DOCKER is set, which turns the skip into a failure.
//
// There is no build tag on these tests on purpose. A build tag keeps the file
// out of the default `go test ./...` and out of `golangci-lint run ./...`, so
// it rots silently. This way the tests are always compiled, always linted, and
// only the container run is conditional - and an environment that is supposed
// to run them cannot quietly stop.
//
// The probe goes through the provider rather than stat-ing
// /var/run/docker.sock, so DOCKER_HOST, rootless docker, colima and podman all
// work.
func requireDocker(t *testing.T) {
	t.Helper()

	provider, err := testcontainers.NewDockerProvider()
	if err == nil {
		defer func() { _ = provider.Close() }()
		err = provider.Health(context.Background())
	}
	if err == nil {
		return
	}

	if os.Getenv("AIOZ_REQUIRE_DOCKER") != "" {
		t.Fatalf("AIOZ_REQUIRE_DOCKER is set but no docker daemon is reachable: %v", err)
	}
	t.Skipf("no docker daemon; set AIOZ_REQUIRE_DOCKER to make this a failure: %v", err)
}

// startRedis runs a redis server and returns its host:port. The container is
// removed when the test finishes.
func startRedis(t *testing.T) string {
	return startRedisWithPassword(t, "")
}

// startRedisWithPassword runs a redis server that demands AUTH. A server with
// no password set accepts any AUTH at all, so a test about credentials has to
// have one configured.
func startRedisWithPassword(t *testing.T, password string) string {
	t.Helper()
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var command []string
	if password != "" {
		command = []string{"redis-server", "--requirepass", password}
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:8-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
			Cmd:          command,
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err)

	endpoint, err := container.Endpoint(ctx, "")
	require.NoError(t, err)

	return endpoint
}
