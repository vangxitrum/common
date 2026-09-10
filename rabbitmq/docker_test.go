package rabbitmq

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The broker's own guest user only accepts loopback connections, and from
// outside the container that is refused with ACCESS_REFUSED - which reads
// exactly like a wrong password. So the container gets a user of its own.
const (
	testUser     = "aioz"
	testPassword = "aioz-test"
)

// requireDocker skips when there is no reachable docker daemon, unless
// AIOZ_REQUIRE_DOCKER is set, which turns the skip into a failure.
//
// There is no build tag on these tests on purpose. A build tag keeps the file
// out of the default `go test ./...` and out of `golangci-lint run ./...`, so
// it rots silently. This way they are always compiled and always linted, only
// the container run is conditional, and an environment that is supposed to run
// them cannot quietly stop.
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

// sharedBroker returns one broker for the whole package.
//
// Starting a broker costs tens of seconds, and most tests only need one that
// works, so they share it and keep their state apart by naming their queues
// after themselves. A test that stops the broker takes its own with
// startBroker.
//
// It is not removed by t.Cleanup: it outlives the test that happened to start
// it. Ryuk, which testcontainers starts alongside, removes it when the test
// session ends.
func sharedBroker(t *testing.T) *broker {
	t.Helper()
	requireDocker(t)

	server, err := sharedBrokerOnce()
	require.NoError(t, err)
	return server
}

var sharedBrokerOnce = sync.OnceValues(newBroker)

// broker is a running RabbitMQ container a test can stop and start again.
type broker struct {
	container testcontainers.Container
	address   string
}

// startBroker runs a broker on a fixed host port.
//
// The port is fixed rather than ephemeral because the reconnect tests stop and
// start the container, and docker hands out a different host port every time
// it starts one with an ephemeral mapping - which would make a reconnect test
// measure the test harness instead of the code.
func startBroker(t *testing.T) *broker {
	t.Helper()
	requireDocker(t)

	server, err := newBroker()
	testcontainers.CleanupContainer(t, containerOf(server))
	require.NoError(t, err)

	return server
}

// containerOf is nil-safe, so a failed start still reaches CleanupContainer.
func containerOf(server *broker) testcontainers.Container {
	if server == nil {
		return nil
	}
	return server.container
}

func newBroker() (*broker, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	hostPort, err := freePort()
	if err != nil {
		return nil, err
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "rabbitmq:4-alpine",
			ExposedPorts: []string{hostPort + ":5672/tcp"},
			Env: map[string]string{
				"RABBITMQ_DEFAULT_USER": testUser,
				"RABBITMQ_DEFAULT_PASS": testPassword,
			},
			WaitingFor: wait.ForListeningPort(nat.Port("5672/tcp")).
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}

	return &broker{container: container, address: "127.0.0.1:" + hostPort}, nil
}

// stop takes the broker away, the way a broker restart or a network partition
// does.
func (b *broker) stop(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	timeout := 10 * time.Second
	require.NoError(t, b.container.Stop(ctx, &timeout))
}

// start brings it back on the same address.
func (b *broker) start(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	require.NoError(t, b.container.Start(ctx))
	require.NoError(t, wait.ForListeningPort(nat.Port("5672/tcp")).
		WithStartupTimeout(2*time.Minute).
		WaitUntilReady(ctx, b.container))
}

// config returns a Config pointing at this broker, with the backoff wound down
// so a reconnect test finishes in seconds.
func (b *broker) config(t *testing.T) Config {
	t.Helper()

	cfg := testConfig()
	cfg.Name = t.Name()
	cfg.Address = b.address
	cfg.Username = testUser
	cfg.Password = testPassword
	cfg.ReconnectDelay = 100 * time.Millisecond
	cfg.ReconnectMaxDelay = time.Second
	cfg.DialTimeout = 5 * time.Second
	return cfg
}

// freePort returns a port nothing is listening on. The gap between closing the
// listener and docker binding the port is a race in theory; in a test process
// that is the only thing asking for ports, it does not happen.
func freePort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = listener.Close() }()

	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), nil
}

// testLogger routes the package's log lines into the test output, so a failure
// shows what the connection was doing.
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
