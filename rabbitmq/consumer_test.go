package rabbitmq

import (
	"context"
	"errors"

	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

func consumerConfig(topology Topology) ConsumerConfig {
	return ConsumerConfig{
		Queue:        topology.Queues[0].Name,
		Prefetch:     4,
		DrainTimeout: 10 * time.Second,
	}
}

// runConsumer starts a consumer and stops it when the test ends.
func runConsumer(t *testing.T, ctx *testcontext.Context, conn *Conn, config ConsumerConfig, topology Topology, handler Handler) *Consumer {
	t.Helper()

	sub := NewConsumer(conn, testLogger(t), config, topology, handler)
	ctx.Go(func() error { return sub.Run(ctx) })
	t.Cleanup(func() { require.NoError(t, sub.Close()) })

	return sub
}

func TestConsumerReceivesAndAcks(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	const messages = 20

	received := make(chan string, messages)
	runConsumer(t, ctx, conn, consumerConfig(topology), topology,
		func(_ context.Context, msg Delivery) error {
			received <- string(msg.Body)
			return nil
		})

	for i := range messages {
		require.NoError(t, pub.Publish(ctx, Message{Body: []byte{byte(i)}}))
	}

	for range messages {
		select {
		case <-received:
		case <-time.After(30 * time.Second):
			t.Fatal("did not receive every message")
		}
	}

	// Acked, so the broker is holding none of them.
	require.Eventually(t, func() bool {
		return queueDepth(t, ctx, conn, topology.Queues[0].Name) == 0
	}, 30*time.Second, 100*time.Millisecond)
}

// A bare error means the message is the problem, so it goes to the broker's
// dead-letter queue instead of round and round the main one.
func TestAHandlerErrorDeadLetters(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	var attempts atomic.Int64
	runConsumer(t, ctx, conn, consumerConfig(topology), topology,
		func(context.Context, Delivery) error {
			attempts.Add(1)
			return errors.New("this message will never work")
		})

	require.NoError(t, pub.Publish(ctx, Message{Body: []byte("poison")}))

	dlq := topology.DeadLetter.Queue
	require.Eventually(t, func() bool {
		return queueDepth(t, ctx, conn, dlq) == 1
	}, 30*time.Second, 100*time.Millisecond)

	// Dead-lettered, not retried: a poison message that keeps coming back is
	// the failure mode this replaces.
	require.Equal(t, int64(1), attempts.Load())
}

// Requeue is the other half of the policy: the world failed, so somebody
// should try this message again.
func TestRequeueRedelivers(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	var attempts atomic.Int64
	succeeded := make(chan struct{})
	runConsumer(t, ctx, conn, consumerConfig(topology), topology,
		func(context.Context, Delivery) error {
			if attempts.Add(1) == 1 {
				return Requeue(errors.New("the database is down"))
			}
			close(succeeded)
			return nil
		})

	require.NoError(t, pub.Publish(ctx, Message{Body: []byte("transient")}))

	select {
	case <-succeeded:
	case <-time.After(30 * time.Second):
		t.Fatal("the requeued message was never redelivered")
	}

	require.Eventually(t, func() bool {
		return queueDepth(t, ctx, conn, topology.DeadLetter.Queue) == 0
	}, 10*time.Second, 100*time.Millisecond)
}

// Concurrency is a bound, not a suggestion: it is what keeps one service from
// opening a hundred database connections because a queue drained at once.
func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	const concurrency = 3

	config := consumerConfig(topology)
	config.Prefetch = 16
	config.Concurrency = concurrency

	var running, peak atomic.Int64
	release := make(chan struct{})
	seen := make(chan struct{}, 32)

	runConsumer(t, ctx, conn, config, topology,
		func(context.Context, Delivery) error {
			current := running.Add(1)
			for {
				top := peak.Load()
				if current <= top || peak.CompareAndSwap(top, current) {
					break
				}
			}
			seen <- struct{}{}
			<-release
			running.Add(-1)
			return nil
		})

	for range 12 {
		require.NoError(t, pub.Publish(ctx, Message{Body: []byte("work")}))
	}

	// Wait until the pool is saturated, then let everything go.
	for range concurrency {
		select {
		case <-seen:
		case <-time.After(30 * time.Second):
			t.Fatal("the pool never filled")
		}
	}
	time.Sleep(500 * time.Millisecond)
	require.LessOrEqual(t, peak.Load(), int64(concurrency))

	close(release)
}

// The shutdown sequence, which is where the two implementations this replaces
// disagreed: work already started finishes, and what the broker had already
// sent goes back on the queue rather than waiting for a consumer timeout.
func TestShutdownDrainsAndLosesNothing(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)
	pub := runPublisher(t, ctx, conn, publisherConfig(topology), topology)

	const messages = 8

	started := make(chan struct{})
	var once sync.Once
	finished := make(chan struct{}, messages)

	config := consumerConfig(topology)
	config.Prefetch = messages
	config.Concurrency = 1

	sub := NewConsumer(conn, testLogger(t), config, topology,
		func(ctx context.Context, _ Delivery) error {
			once.Do(func() { close(started) })
			// Long enough that the shutdown certainly happens underneath it.
			time.Sleep(time.Second)
			// The handler's context outlives the shutdown, which is the whole
			// point of draining rather than canceling.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			finished <- struct{}{}
			return nil
		})

	consumerDone := make(chan error, 1)
	go func() { consumerDone <- sub.Run(ctx) }()

	for range messages {
		require.NoError(t, pub.Publish(ctx, Message{Body: []byte("work")}))
	}

	<-started
	require.NoError(t, sub.Close())

	select {
	case err := <-consumerDone:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the consumer did not stop")
	}

	// The one in flight completed rather than being torn up.
	require.Len(t, finished, 1)

	// And nothing was lost: everything the handler did not finish is back on
	// the queue, not stuck unacked until the broker's timeout.
	require.Eventually(t, func() bool {
		return queueDepth(t, ctx, conn, topology.Queues[0].Name) == messages-1
	}, 30*time.Second, 200*time.Millisecond)
}

// Re-declaring a queue with different arguments is the failure that looks like
// a network fault: the broker kills the channel, the topology is re-declared
// on every reconnect, and the service loops. The error has to say what it
// really is.
func TestAChangedQueueArgumentIsAClearError(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	conn := run(t, ctx, sharedBroker(t).config(t))
	topology := topologyFor(t)
	topology.DeadLetter = DeadLetter{}

	ch, err := conn.Channel(ctx)
	require.NoError(t, err)
	_, err = ch.QueueDeclare(topology.Queues[0].Name, true, false, false, false,
		amqp.Table{"x-message-ttl": int32(60000)})
	require.NoError(t, err)
	require.NoError(t, ch.Close())

	// The topology declares that same queue without the TTL.
	second, err := conn.Channel(ctx)
	require.NoError(t, err)
	defer func() { _ = second.Close() }()

	err = topology.Declare(ctx, second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists with different arguments")
	require.Contains(t, err.Error(), "re-declared on every reconnect")
}
