package rabbitmq

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// PublisherConfig describes how a Publisher sends.
type PublisherConfig struct {
	Exchange   string `help:"exchange to publish to; empty is the default exchange" default:""`
	RoutingKey string `help:"default routing key when Publish does not set one" default:""`

	Confirms       bool          `help:"wait for a broker confirm on every publish" default:"true"`
	ConfirmTimeout time.Duration `help:"how long to wait for a confirm" default:"10s"`
	Mandatory      bool          `help:"return a message the broker cannot route, instead of dropping it" default:"false"`
	Persistent     bool          `help:"mark messages persistent so they survive a broker restart" default:"true"`

	Retries    int           `help:"publish attempts after a channel failure; 0 disables retrying" default:"3"`
	RetryDelay time.Duration `help:"wait before the first retry; doubles up to the reconnect maximum" default:"200ms"`
}

// DefaultConfirmTimeout bounds the wait for a confirm when the config leaves
// ConfirmTimeout at zero.
const DefaultConfirmTimeout = 10 * time.Second

// returnGrace is how long a mandatory publish waits, after its confirm, for a
// return that may already be on its way.
//
// The broker sends basic.return before basic.ack for the same message, so by
// the time the confirm lands the return frame has been read; but the library
// delivers returns on a separate channel, so the goroutine that files it may
// not have run yet. Without this window an unroutable message would sometimes
// be reported as sent. It costs a routable mandatory publish this much only
// when the file is genuinely late, since the wait ends as soon as it arrives,
// and mandatory publishing is off by default.
const returnGrace = 25 * time.Millisecond

// Publisher sends messages on a confirming channel that survives reconnects.
//
// Run holds the channel; Publish uses it. A publish issued while the broker is
// away blocks until there is a channel again rather than failing, so a broker
// restart is a pause instead of a wave of dropped messages - which is what
// every one of the wrappers this replaces got wrong in its own way.
type Publisher struct {
	conn     *Conn
	log      *slog.Logger
	config   PublisherConfig
	topology Topology
	name     monkit.SeriesTag

	duration *monkit.DurationVal

	mu    sync.Mutex
	ch    *amqp.Channel
	ready chan struct{} // closed while ch is live; replaced on disconnect

	// returns holds a waiter per in-flight mandatory publish, keyed by message
	// id. A basic.return arrives before the confirm, so a publish that was
	// returned can be reported as an error rather than silently vanishing.
	returns map[string]chan amqp.Return

	done      chan struct{}
	closeOnce sync.Once

	sent, failed, nacked, returned, retried int64
}

// NewPublisher returns a Publisher. It does nothing until Run is called.
//
// The topology is declared on the publisher's own channel, and re-declared on
// every reconnect: publishing to an exchange that a re-created broker has
// never heard of is a silent discard.
func NewPublisher(conn *Conn, log *slog.Logger, config PublisherConfig, topology Topology) *Publisher {
	name := monkit.NewSeriesTag("name", conn.config.name())

	return &Publisher{
		conn:     conn,
		log:      log,
		config:   config,
		topology: topology,
		name:     name,
		duration: mon.DurationVal("rabbitmq_publish", name),
		ready:    make(chan struct{}),
		returns:  map[string]chan amqp.Return{},
		done:     make(chan struct{}),
	}
}

// Run holds one confirming channel for as long as the connection lives, and
// takes a new one every time it does not.
func (pub *Publisher) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	return pub.conn.Session(ctx, func(ctx context.Context, ch *amqp.Channel) error {
		if err := pub.topology.Declare(ctx, ch); err != nil {
			return err
		}
		if pub.config.Confirms {
			if err := ch.Confirm(false); err != nil {
				return Error.Errorf("putting the channel in confirm mode: %w", err)
			}
		}

		var watching chan struct{}
		if pub.config.Mandatory {
			watching = pub.watchReturns(ctx, ch)
		}

		pub.publishChannel(ch)
		defer pub.retractChannel()

		<-ctx.Done()
		if watching != nil {
			<-watching
		}
		// Returning the reason matters. This context is canceled either
		// because the service is stopping or because the channel died, and
		// Session tells those apart by the error: nil would end the session
		// for good and leave every later publish waiting for a channel that
		// is never coming back.
		return ctx.Err()
	})
}

// watchReturns dispatches basic.return to whichever publish is waiting for it.
func (pub *Publisher) watchReturns(ctx context.Context, ch *amqp.Channel) chan struct{} {
	returns := ch.NotifyReturn(make(chan amqp.Return, 16))
	watching := make(chan struct{})

	go func() {
		defer close(watching)
		for {
			select {
			case ret, ok := <-returns:
				if !ok {
					return
				}
				pub.deliverReturn(ret)
			case <-ctx.Done():
				return
			}
		}
	}()

	return watching
}

func (pub *Publisher) deliverReturn(ret amqp.Return) {
	pub.mu.Lock()
	waiter := pub.returns[ret.MessageId]
	pub.mu.Unlock()

	if waiter == nil {
		pub.log.Warn("rabbitmq returned a message nobody was waiting for",
			slog.String("exchange", ret.Exchange),
			slog.String("routing_key", ret.RoutingKey),
			slog.String("message_id", ret.MessageId))
		return
	}

	select {
	case waiter <- ret:
	default:
	}
}

func (pub *Publisher) publishChannel(ch *amqp.Channel) {
	pub.mu.Lock()
	defer pub.mu.Unlock()

	pub.ch = ch
	close(pub.ready)
}

func (pub *Publisher) retractChannel() {
	pub.mu.Lock()
	defer pub.mu.Unlock()

	pub.ch = nil
	pub.ready = make(chan struct{})
}

// Close stops Publish from waiting for a channel that is not coming back.
// The channel itself belongs to Run and is closed when Run returns.
func (pub *Publisher) Close() error {
	pub.closeOnce.Do(func() { close(pub.done) })
	return nil
}

// PublishJSON marshals v and publishes it as application/json.
//
// It is the shape five of the six wrappers this replaces hand-wrote, content
// type and all.
func (pub *Publisher) PublishJSON(ctx context.Context, routingKey string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return Error.Wrap(err)
	}

	return pub.Publish(ctx, Message{
		RoutingKey:  routingKey,
		Body:        body,
		ContentType: "application/json",
	})
}

// Publish sends one message and waits for the broker to confirm it.
//
// It is safe to call from many goroutines. Each publish waits on its own
// deferred confirmation, bound to its own delivery tag, so concurrent
// publishers cannot consume each other's acks - the bug that let a failed
// publish report success in one of the wrappers this replaces.
//
// A channel failure is retried: the message goes out again on the next
// channel, up to Retries. A broker NACK is not. The broker rejected the
// message, so sending it again produces the same rejection; that is an error
// the caller has to decide about.
func (pub *Publisher) Publish(ctx context.Context, msg Message) (err error) {
	defer mon.Task()(&ctx)(&err)

	if pub.config.Mandatory && msg.MessageID == "" {
		// A return is matched by message id, so a mandatory publish has to
		// have one before the body is rendered.
		msg.MessageID = mustUUID()
	}

	publishing, err := msg.publishing(pub.config.Persistent)
	if err != nil {
		return err
	}

	routingKey := msg.RoutingKey
	if routingKey == "" {
		routingKey = pub.config.RoutingKey
	}

	delay := pub.config.RetryDelay
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}

	attempts := pub.config.Retries + 1
	if attempts < 1 {
		attempts = 1
	}

	start := time.Now()
	defer func() { pub.duration.Observe(time.Since(start)) }()

	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			pub.count(&pub.retried)
			mon.Event("rabbitmq_publish_retry", pub.name)
			if !time2.Sleep(ctx, delay) {
				return Error.Wrap(ctx.Err())
			}
			delay = min(delay*2, pub.maxDelay())
		}

		retry, err := pub.publishOnce(ctx, routingKey, publishing)
		if err == nil {
			pub.count(&pub.sent)
			return nil
		}
		lastErr = err

		if !retry {
			pub.count(&pub.failed)
			return err
		}
	}

	pub.count(&pub.failed)
	mon.Event("rabbitmq_publish_failed", pub.name)
	return Error.Errorf("publish failed after %d attempts: %w", attempts, lastErr)
}

// publishOnce sends on the current channel and waits for its confirm. It
// reports whether the failure is one a new channel would fix.
func (pub *Publisher) publishOnce(ctx context.Context, routingKey string, publishing amqp.Publishing) (retry bool, err error) {
	ch, err := pub.channel(ctx)
	if err != nil {
		return false, err
	}

	var returned chan amqp.Return
	if pub.config.Mandatory {
		returned = pub.registerReturn(publishing.MessageId)
		defer pub.forgetReturn(publishing.MessageId)
	}

	// The write is serialized because an amqp channel is not safe for
	// concurrent use. The wait is not: it happens after the lock is released,
	// which is what lets many publishers be in flight at once.
	pub.mu.Lock()
	confirm, err := ch.PublishWithDeferredConfirmWithContext(
		ctx, pub.config.Exchange, routingKey, pub.config.Mandatory, false, publishing)
	pub.mu.Unlock()

	if err != nil {
		return isBrokerError(err), Error.Errorf("publishing to %q: %w", pub.config.Exchange, err)
	}
	if !pub.config.Confirms || confirm == nil {
		return false, nil
	}

	confirmCtx, cancel := context.WithTimeout(ctx, pub.confirmTimeout())
	defer cancel()

	acked, err := confirm.WaitContext(confirmCtx)
	if err != nil {
		// The channel died under the wait, or the caller gave up. A new
		// channel would resolve the first; the second is not ours to retry.
		return ctx.Err() == nil, Error.Errorf("waiting for confirm of delivery tag %d: %w", confirm.DeliveryTag, err)
	}
	if !acked {
		pub.count(&pub.nacked)
		mon.Event("rabbitmq_publish_nacked", pub.name)
		return false, Error.Errorf("broker nacked delivery tag %d", confirm.DeliveryTag)
	}

	if returned != nil {
		timer := time.NewTimer(returnGrace)
		defer timer.Stop()

		select {
		case ret := <-returned:
			pub.count(&pub.returned)
			mon.Event("rabbitmq_publish_returned", pub.name)
			return false, Error.Errorf(
				"broker could not route message to any queue: exchange %q, routing key %q, %d %s",
				ret.Exchange, ret.RoutingKey, ret.ReplyCode, ret.ReplyText)
		case <-timer.C:
		case <-ctx.Done():
		}
	}

	return false, nil
}

// channel returns the live publishing channel, waiting for one if the
// connection is down.
func (pub *Publisher) channel(ctx context.Context) (*amqp.Channel, error) {
	for {
		pub.mu.Lock()
		ch, ready := pub.ch, pub.ready
		pub.mu.Unlock()

		if ch != nil {
			return ch, nil
		}

		select {
		case <-ready:
		case <-ctx.Done():
			return nil, Error.Wrap(ctx.Err())
		case <-pub.done:
			return nil, Error.Errorf("publisher is closed")
		}
	}
}

func (pub *Publisher) registerReturn(messageID string) chan amqp.Return {
	waiter := make(chan amqp.Return, 1)

	pub.mu.Lock()
	defer pub.mu.Unlock()

	pub.returns[messageID] = waiter
	return waiter
}

func (pub *Publisher) forgetReturn(messageID string) {
	pub.mu.Lock()
	defer pub.mu.Unlock()

	delete(pub.returns, messageID)
}

func (pub *Publisher) confirmTimeout() time.Duration {
	if pub.config.ConfirmTimeout > 0 {
		return pub.config.ConfirmTimeout
	}
	return DefaultConfirmTimeout
}

func (pub *Publisher) maxDelay() time.Duration {
	if pub.conn.config.ReconnectMaxDelay > 0 {
		return pub.conn.config.ReconnectMaxDelay
	}
	return defaultReconnectMaxDelay
}

func (pub *Publisher) count(field *int64) {
	pub.mu.Lock()
	defer pub.mu.Unlock()

	*field++
}

// Stats implements monkit.StatSource. There is no publish instrumentation
// anywhere in the estate today, and this is the only place it can come from
// now that the package owns the publish.
func (pub *Publisher) Stats(cb func(key monkit.SeriesKey, field string, val float64)) {
	key := monkit.NewSeriesKey("rabbitmq_publisher").WithTags(pub.name)

	pub.mu.Lock()
	connected := pub.ch != nil
	sent, failed, nacked, returned, retried := pub.sent, pub.failed, pub.nacked, pub.returned, pub.retried
	pub.mu.Unlock()

	cb(key, "ready", boolAsFloat(connected))
	cb(key, "sent", float64(sent))
	cb(key, "failed", float64(failed))
	cb(key, "nacked", float64(nacked))
	cb(key, "returned", float64(returned))
	cb(key, "retried", float64(retried))
}
