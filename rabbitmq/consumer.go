package rabbitmq

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/context2"
)

// ConsumerConfig describes how a Consumer receives.
type ConsumerConfig struct {
	Queue       string `help:"queue to consume from"`
	ConsumerTag string `help:"consumer tag reported to the broker; empty generates one" default:""`

	Prefetch    int  `help:"unacknowledged messages the broker may send; values below 1 are treated as 1" default:"16"`
	Concurrency int  `help:"handlers running at once; 0 uses Prefetch" default:"0"`
	AutoAck     bool `help:"acknowledge on delivery instead of after the handler returns; loses messages on a crash" default:"false"`

	RequeueOnError bool          `help:"requeue a message whose handler failed, instead of dead-lettering it" default:"false"`
	DrainTimeout   time.Duration `help:"how long to let in-flight handlers finish at shutdown" default:"30s"`
}

// DefaultDrainTimeout bounds the shutdown drain when the config leaves
// DrainTimeout at zero.
const DefaultDrainTimeout = 30 * time.Second

// Handler processes one delivery.
//
// Returning nil acks it. Returning an error wrapped in Requeue nacks it with
// requeue set, so another consumer may take it. Any other error nacks it
// without requeue, which sends it to the queue's dead-letter exchange if it
// has one and drops it if it does not.
//
// That split is the whole ack policy, and it is the caller's to make: only the
// handler knows whether its failure was the message (poison, retry forever)
// or the world (transient, try again).
//
// The context is not canceled by a shutdown. Work that has already started
// finishes rather than being torn up halfway through a write, which is why the
// consumer drains instead of canceling. It is bounded by DrainTimeout
// instead, so a handler that never returns cannot hold the shutdown open.
type Handler func(ctx context.Context, msg Delivery) error

// Delivery is one received message.
//
// The embedded amqp.Delivery is the full API. Ack, Nack and Reject are handled
// by the consumer from the handler's return value, so a handler must not call
// them: a double ack kills the channel.
type Delivery struct {
	amqp.Delivery
}

// requeueError marks a failure as transient.
type requeueError struct{ err error }

func (e requeueError) Error() string { return e.err.Error() }
func (e requeueError) Unwrap() error { return e.err }

// Requeue wraps err to say the message should go back on the queue.
//
// Use it when the failure is the world's - a database that is down, a rate
// limit, a timeout. Return a bare error when the failure is the message's,
// because requeueing a message that can never be processed is an infinite
// loop with a dead-letter queue standing empty beside it.
func Requeue(err error) error {
	if err == nil {
		return nil
	}
	return requeueError{err: err}
}

// IsRequeue reports whether err asks for a requeue, anywhere in its chain.
func IsRequeue(err error) bool {
	var target requeueError
	return errors.As(err, &target)
}

// Consumer receives from one queue into a bounded pool of handlers.
//
// It re-declares the topology and re-registers itself on every reconnect, and
// at shutdown it stops the broker sending more, nacks what it was already
// sent, and lets the handlers already running finish.
type Consumer struct {
	conn     *Conn
	log      *slog.Logger
	config   ConsumerConfig
	topology Topology
	handler  Handler
	name     monkit.SeriesTag

	duration *monkit.DurationVal

	mu       sync.Mutex
	inflight int64

	handled, failed, requeued, dropped int64

	done      chan struct{}
	closeOnce sync.Once
}

// NewConsumer returns a Consumer. It does nothing until Run is called.
func NewConsumer(conn *Conn, log *slog.Logger, config ConsumerConfig, topology Topology, handler Handler) *Consumer {
	name := monkit.NewSeriesTag("name", conn.config.name())

	return &Consumer{
		conn:     conn,
		log:      log,
		config:   config,
		topology: topology,
		handler:  handler,
		name:     name,
		duration: mon.DurationVal("rabbitmq_deliver", name),
		done:     make(chan struct{}),
	}
}

// Run consumes until ctx is done or Close is called, across as many
// connections as that takes.
func (c *Consumer) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	if c.config.Queue == "" {
		return Error.Errorf("consumer has no queue")
	}
	if c.handler == nil {
		return Error.Errorf("consumer has no handler")
	}

	// Close is what stops a consumer that a service is shutting down
	// independently of its context, so the session has to watch it too.
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-sessionCtx.Done():
		}
	}()

	return c.conn.Session(sessionCtx, c.consume)
}

// consume is one channel's worth of consuming: declare, register, pump, drain.
func (c *Consumer) consume(ctx context.Context, ch *amqp.Channel) error {
	if err := c.topology.Declare(ctx, ch); err != nil {
		return err
	}

	prefetch := c.config.Prefetch
	if prefetch < 1 {
		// A prefetch of zero is unlimited, which is never what a bounded pool
		// wants: the broker would hand over the whole queue.
		prefetch = 1
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return Error.Errorf("setting prefetch: %w", err)
	}

	tag := c.config.ConsumerTag
	if tag == "" {
		tag = c.conn.config.name() + "-" + mustUUID()
	}

	deliveries, err := ch.ConsumeWithContext(ctx, c.config.Queue, tag,
		c.config.AutoAck, false, false, false, nil)
	if err != nil {
		return Error.Errorf("consuming from %q: %w", c.config.Queue, err)
	}

	mon.Event("rabbitmq_consumer_started", c.name)
	c.log.Info("consuming",
		slog.String("queue", c.config.Queue),
		slog.String("consumer_tag", tag),
		slog.Int("prefetch", prefetch),
		slog.Int("concurrency", c.concurrency()))

	return c.pump(ctx, ch, tag, deliveries)
}

// pump hands deliveries to a bounded worker pool and drains at the end.
func (c *Consumer) pump(ctx context.Context, ch *amqp.Channel, tag string, deliveries <-chan amqp.Delivery) error {
	// The pool is a semaphore plus a WaitGroup, which is the shape two of the
	// services this replaces had already converged on independently.
	sem := make(chan struct{}, c.concurrency())
	var workers sync.WaitGroup

	defer func() {
		c.drain(ch, tag, deliveries, &workers)
	}()

	for {
		select {
		case <-ctx.Done():
			// The reason is the whole message: Session ends when the service
			// is stopping and starts a new consumer when it was the channel
			// that died, and it can only tell those apart from the error.
			return ctx.Err()

		case delivery, ok := <-deliveries:
			if !ok {
				// The broker or the channel closed the stream. Session
				// decides whether that is a reconnect.
				return Error.Errorf("delivery channel closed")
			}

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				// Shutting down with the pool full: give this one back rather
				// than holding it until the broker's timeout.
				c.nack(delivery, true)
				return ctx.Err()
			}

			workers.Go(func() {
				defer func() { <-sem }()
				c.handle(ctx, delivery)
			})
		}
	}
}

// drain is the shutdown sequence, in the order that loses nothing.
//
// Cancel first so the broker stops sending. Then nack what it already sent and
// nobody has picked up, because a message left unacked waits for the broker's
// consumer timeout before anyone else sees it. Then wait for the handlers that
// are already running, bounded, and say so if they do not finish.
func (c *Consumer) drain(ch *amqp.Channel, tag string, deliveries <-chan amqp.Delivery, workers *sync.WaitGroup) {
	if err := ch.Cancel(tag, false); err != nil && !errors.Is(err, amqp.ErrClosed) {
		c.log.Warn("canceling the consumer failed",
			slog.String("queue", c.config.Queue),
			slog.String("error", err.Error()))
	}

	for {
		select {
		case delivery, ok := <-deliveries:
			if !ok {
				c.waitForWorkers(workers)
				return
			}
			c.nack(delivery, true)
		default:
			c.waitForWorkers(workers)
			return
		}
	}
}

func (c *Consumer) waitForWorkers(workers *sync.WaitGroup) {
	finished := make(chan struct{})
	go func() {
		workers.Wait()
		close(finished)
	}()

	timeout := c.config.DrainTimeout
	if timeout <= 0 {
		timeout = DefaultDrainTimeout
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-finished:
	case <-timer.C:
		c.log.Warn("handlers did not finish within the drain timeout",
			slog.String("queue", c.config.Queue),
			slog.Duration("timeout", timeout),
			slog.Int64("in_flight", c.snapshotInflight()))
		mon.Event("rabbitmq_drain_timeout", c.name)
	}
}

// handle runs the handler for one delivery and acks by its outcome.
func (c *Consumer) handle(ctx context.Context, delivery amqp.Delivery) {
	// Work already started finishes rather than being torn up mid-write: the
	// handler's context loses the shutdown cancellation and gains the drain
	// bound in its place. That bound is the part crawler-service's
	// context.WithoutCancel is missing, and it is what keeps a handler that
	// never returns from holding the shutdown open forever.
	handlerCtx, cancel := context2.WithRetimeout(ctx, c.drainTimeout())
	defer cancel()

	c.enter()
	start := time.Now()
	err := c.safely(handlerCtx, Delivery{Delivery: delivery})
	c.duration.Observe(time.Since(start))
	c.leave()

	if c.config.AutoAck {
		if err != nil {
			c.count(&c.failed)
			mon.Event("rabbitmq_deliver_failed", c.name)
		}
		return
	}

	switch {
	case err == nil:
		c.count(&c.handled)
		if ackErr := delivery.Ack(false); ackErr != nil {
			c.log.Warn("acking a delivery failed",
				slog.String("queue", c.config.Queue),
				slog.String("error", ackErr.Error()))
		}

	case IsRequeue(err) || c.config.RequeueOnError || errors.Is(err, context.Canceled):
		// A cancellation is always a requeue: the handler did not decide the
		// message was bad, it was interrupted.
		c.count(&c.requeued)
		mon.Event("rabbitmq_deliver_requeued", c.name)
		c.logHandlerError(delivery, err, "requeueing")
		c.nack(delivery, true)

	default:
		c.count(&c.dropped)
		mon.Event("rabbitmq_deliver_dropped", c.name)
		c.logHandlerError(delivery, err, "dead-lettering")
		c.nack(delivery, false)
	}
}

// safely runs the handler and turns a panic into an error, so one bad message
// takes down one delivery rather than the process.
func (c *Consumer) safely(ctx context.Context, msg Delivery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Error.Errorf("handler panicked: %v", r)
		}
	}()

	return c.handler(ctx, msg)
}

func (c *Consumer) logHandlerError(delivery amqp.Delivery, err error, action string) {
	c.count(&c.failed)
	mon.Event("rabbitmq_deliver_failed", c.name)
	c.log.Warn("handler failed; "+action,
		slog.String("queue", c.config.Queue),
		slog.String("message_id", delivery.MessageId),
		slog.String("routing_key", delivery.RoutingKey),
		slog.String("error", err.Error()))
}

func (c *Consumer) nack(delivery amqp.Delivery, requeue bool) {
	if c.config.AutoAck {
		return
	}
	if err := delivery.Nack(false, requeue); err != nil && !errors.Is(err, amqp.ErrClosed) {
		c.log.Warn("nacking a delivery failed",
			slog.String("queue", c.config.Queue),
			slog.Bool("requeue", requeue),
			slog.String("error", err.Error()))
	}
}

// Close stops the consumer. Run returns once the drain finishes.
func (c *Consumer) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

func (c *Consumer) concurrency() int {
	if c.config.Concurrency > 0 {
		return c.config.Concurrency
	}
	if c.config.Prefetch > 0 {
		return c.config.Prefetch
	}
	return 1
}

func (c *Consumer) drainTimeout() time.Duration {
	if c.config.DrainTimeout > 0 {
		return c.config.DrainTimeout
	}
	return DefaultDrainTimeout
}

func (c *Consumer) enter() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.inflight++
}

func (c *Consumer) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.inflight--
}

func (c *Consumer) snapshotInflight() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.inflight
}

func (c *Consumer) count(field *int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	*field++
}

// Stats implements monkit.StatSource.
func (c *Consumer) Stats(cb func(key monkit.SeriesKey, field string, val float64)) {
	key := monkit.NewSeriesKey("rabbitmq_consumer").
		WithTags(c.name).
		WithTag("queue", c.config.Queue)

	c.mu.Lock()
	inflight, handled, failed, requeued, dropped := c.inflight, c.handled, c.failed, c.requeued, c.dropped
	c.mu.Unlock()

	cb(key, "in_flight", float64(inflight))
	cb(key, "handled", float64(handled))
	cb(key, "failed", float64(failed))
	cb(key, "requeued", float64(requeued))
	cb(key, "dropped", float64(dropped))
}
