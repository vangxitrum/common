package rabbitmq

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

// topologyFor returns a private exchange, queue and dead-letter path named
// after the test, so the shared broker can carry every test at once.
func topologyFor(t *testing.T) Topology {
	t.Helper()

	// Durable, because RabbitMQ 4 refuses a transient non-exclusive queue
	// outright: the declaration fails with INTERNAL_ERROR and, since the
	// topology is re-declared on every reconnect, the connection loops.
	// The suffix keeps two runs of the same test apart. The queues are durable
	// and the broker is shared, so a name derived only from the test would let
	// the previous run's leftovers arrive in this one.
	name := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + mustUUID()
	return Topology{
		Exchanges: []Exchange{{Name: name, Kind: "topic", Durable: true}},
		Queues:    []Queue{{Name: name, Durable: true}},
		Bindings:  []Binding{{Exchange: name, Queue: name, RoutingKey: "key"}},
		DeadLetter: DeadLetter{
			Exchange: name + ".dlx",
			Queue:    name + ".dlq",
		},
	}
}

func publisherConfig(topology Topology) PublisherConfig {
	return PublisherConfig{
		Exchange:       topology.Exchanges[0].Name,
		RoutingKey:     "key",
		Confirms:       true,
		ConfirmTimeout: 10 * time.Second,
		Persistent:     true,
		Retries:        3,
		RetryDelay:     100 * time.Millisecond,
	}
}

// runPublisher starts a publisher and waits until it holds a channel.
func runPublisher(t *testing.T, ctx *testcontext.Context, conn *Conn, config PublisherConfig, topology Topology) *Publisher {
	t.Helper()

	pub := NewPublisher(conn, testLogger(t), config, topology)
	ctx.Go(func() error { return pub.Run(ctx) })
	t.Cleanup(func() { require.NoError(t, pub.Close()) })

	_, err := pub.channel(ctx)
	require.NoError(t, err)

	return pub
}

func TestPublishAndReceive(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	server := sharedBroker(t)
	conn := run(t, ctx, server.config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	require.NoError(t, pub.PublishJSON(ctx, "key", map[string]string{"hello": "world"}))

	delivery := getOne(t, ctx, conn, topology.Queues[0].Name)
	require.JSONEq(t, `{"hello":"world"}`, string(delivery.Body))
	require.Equal(t, "application/json", delivery.ContentType)
	require.Equal(t, uint8(amqp.Persistent), delivery.DeliveryMode)
	require.NotEmpty(t, delivery.MessageId)
}

// The regression test for the bug this package was written to remove: a shared
// confirm channel with no correlation to a delivery tag lets concurrent
// publishers consume each other's acks, so a publish that failed can report
// success. Every one of these has to get its own confirm.
func TestConcurrentPublishersEachGetTheirOwnConfirm(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	const publishers = 32

	var group sync.WaitGroup
	errCh := make(chan error, publishers)
	for i := range publishers {
		group.Go(func() {
			errCh <- pub.Publish(ctx, Message{Body: []byte{byte(i)}})
		})
	}
	group.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	// Every publish that reported success is on the queue. A stolen confirm
	// shows up here as a message that was acked but never arrived.
	queue := topology.Queues[0].Name
	require.Eventually(t, func() bool {
		return queueDepth(t, ctx, conn, queue) == publishers
	}, 30*time.Second, 100*time.Millisecond)
}

// A publish issued while the broker is away waits for it to come back, rather
// than failing. That is the difference between a broker restart being a pause
// and being an incident.
func TestPublishWaitsForTheBroker(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	server := startBroker(t)
	conn := run(t, ctx, server.config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	server.stop(t)
	require.Eventually(t, func() bool { return !conn.Connected() }, time.Minute, 50*time.Millisecond)

	published := make(chan error, 1)
	go func() { published <- pub.Publish(ctx, Message{Body: []byte("after the outage")}) }()

	// It must still be waiting: nothing has come back yet.
	select {
	case err := <-published:
		t.Fatalf("publish returned while the broker was down: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	server.start(t)

	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(2 * time.Minute):
		t.Fatal("publish did not complete after the broker came back")
	}
}

// A NACK is the broker rejecting the message. Sending it again produces the
// same rejection, so it is an error the caller has to see, not something to
// retry.
func TestANackIsNotRetried(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))

	topology := topologyFor(t)
	// A queue that holds one message and rejects publishes past that is the
	// only reliable way to make a broker nack on demand.
	topology.Queues[0].Args = amqp.Table{
		"x-max-length": int32(1),
		"x-overflow":   "reject-publish",
	}
	topology.DeadLetter = DeadLetter{}

	config := publisherConfig(topology)
	config.Retries = 2
	pub := runPublisher(t, ctx, conn, config, topology)

	require.NoError(t, pub.Publish(ctx, Message{Body: []byte("fits")}))

	start := time.Now()
	err := pub.Publish(ctx, Message{Body: []byte("does not fit")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nacked")

	// Retrying twice with a 100ms backoff would have taken noticeably longer.
	require.Less(t, time.Since(start), 2*time.Second)
}

// Mandatory publishing exists so an unroutable message is an error rather than
// a silent discard. No wrapper in the estate does this today.
func TestAMandatoryPublishReportsAnUnroutableMessage(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)

	config := publisherConfig(topology)
	config.Mandatory = true
	pub := runPublisher(t, ctx, conn, config, topology)

	err := pub.Publish(ctx, Message{RoutingKey: "nothing.is.bound.to.this", Body: []byte("lost")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not route")
}

// getOne takes a single message off a queue, waiting for it to arrive.
func getOne(t *testing.T, ctx context.Context, conn *Conn, queue string) amqp.Delivery {
	t.Helper()

	ch, err := conn.Channel(ctx)
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		delivery, ok, err := ch.Get(queue, true)
		require.NoError(t, err)
		if ok {
			return delivery
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("no message arrived on %q", queue)
	return amqp.Delivery{}
}

// queueDepth is the broker's own count of ready messages.
func queueDepth(t *testing.T, ctx context.Context, conn *Conn, queue string) int {
	t.Helper()

	ch, err := conn.Channel(ctx)
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()

	// Passive: report the queue as it is, rather than declaring it.
	state, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	require.NoError(t, err)

	return state.Messages
}
