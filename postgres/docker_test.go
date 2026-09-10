package postgres_test

import (
	"context"
	"os"
	"strconv"
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

const (
	testDatabase = "aioz"
	testUser     = "aioz"
	testPassword = "hunter2"
)

// startPostgres runs a server and returns its host and port. The container is
// removed when the test finishes.
func startPostgres(t *testing.T) (host string, port int) {
	t.Helper()
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := testcontainers.GenericContainer(
		ctx,
		testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "postgres:17-alpine",
				ExposedPorts: []string{"5432/tcp"},
				Env: map[string]string{
					"POSTGRES_DB":       testDatabase,
					"POSTGRES_USER":     testUser,
					"POSTGRES_PASSWORD": testPassword,
				},
				// Occurrence(2) is not a belt-and-braces flourish: the official
				// image starts Postgres once to run initdb and the entrypoint
				// scripts, shuts it down, and starts it again. Waiting on the port
				// or on the first ready line connects to the initdb instance and
				// the test flakes when that one is stopped underneath it.
				WaitingFor: wait.ForAll(
					wait.ForListeningPort("5432/tcp"),
					wait.ForLog("database system is ready to accept connections").
						WithOccurrence(2),
				),
			},
			Started: true,
		},
	)
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err)

	host, err = container.Host(ctx)
	require.NoError(t, err)

	mapped, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)

	port, err = strconv.Atoi(mapped.Port())
	require.NoError(t, err)

	return host, port
}
