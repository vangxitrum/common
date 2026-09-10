package rabbitmq

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/errs2"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// Conn is a supervised AMQP connection.
//
// It is not connected until Run is called, and it reconnects for as long as
// Run is running. Everything else here waits for a live connection rather than
// failing when there is not one, because a broker that is briefly gone is the
// normal case this package exists for.
type Conn struct {
	log    *slog.Logger
	config Config
	url    string
	name   monkit.SeriesTag

	mu    sync.Mutex
	conn  *amqp.Connection
	ready chan struct{} // closed while conn is live; replaced on disconnect
	gen   int64         // connection generation, for metrics and logs

	done      chan struct{}
	closeOnce sync.Once
}

// New returns a Conn for the config. It does not connect: call Run.
func New(log *slog.Logger, config Config) *Conn {
	return NewWithURL(log, config.DialURL(), config)
}

// NewWithURL is New for a caller whose broker URL arrives from somewhere the
// Config cannot express - a platform binding, a secrets manager. The config
// still supplies the timeouts, backoff and name.
func NewWithURL(log *slog.Logger, url string, config Config) *Conn {
	return &Conn{
		log:    log,
		config: config,
		url:    url,
		name:   monkit.NewSeriesTag("name", config.name()),
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Run connects and keeps connecting until ctx is done or Close is called.
//
// A context cancellation is how a service is asked to stop, so that returns
// nil. The only error it returns is a first connection that failed while
// RequireOnStart is set; after the first success it retries forever, because a
// broker down for ten minutes is an outage to ride out rather than a reason to
// exit.
func (conn *Conn) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	delay := conn.config.ReconnectDelay
	if delay <= 0 {
		delay = defaultReconnectDelay
	}
	maxDelay := conn.config.ReconnectMaxDelay
	if maxDelay <= 0 {
		maxDelay = defaultReconnectMaxDelay
	}

	first := true
	wait := delay

	for {
		if stopped(ctx, conn.done) {
			return nil
		}

		connected, err := conn.runOnce(ctx)
		switch {
		case connected:
			// A connection that came up and later died starts the backoff
			// over: the next outage is a new one, not a continuation.
			wait = delay
		case first && conn.config.RequireOnStart:
			return Error.Errorf("connecting to the broker at startup: %w", err)
		}
		first = false

		if err != nil && !stopped(ctx, conn.done) {
			conn.log.Warn("rabbitmq connection lost; reconnecting",
				slog.String("name", conn.config.name()),
				slog.String("address", conn.config.Address),
				slog.Duration("in", wait),
				slog.String("error", err.Error()))
			mon.Event("rabbitmq_connection_lost", conn.name)
		}

		if !conn.sleep(ctx, wait) {
			return nil
		}

		wait *= 2
		if wait > maxDelay {
			wait = maxDelay
		}
	}
}

// runOnce dials, holds the connection until it dies, and reports whether it
// was ever live.
func (conn *Conn) runOnce(ctx context.Context) (connected bool, err error) {
	amqpConfig, err := conn.config.amqp()
	if err != nil {
		return false, err
	}

	c, err := amqp.DialConfig(conn.url, amqpConfig)
	if err != nil {
		mon.Event("rabbitmq_connect_failed", conn.name)
		return false, Error.Wrap(err)
	}

	// Buffered on purpose. amqp091 sends the close reason on this channel
	// during shutdown and blocks if nobody is receiving, so an unbuffered one
	// deadlocks the connection it is watching. It is also a local variable per
	// attempt rather than a field, which is what keeps a second reconnect from
	// watching the first attempt's dead channel.
	closed := c.NotifyClose(make(chan *amqp.Error, 1))

	conn.publish(c)
	defer conn.retract()

	conn.log.Info("connected to rabbitmq",
		slog.String("name", conn.config.name()),
		slog.String("address", conn.config.Address),
		slog.String("vhost", amqpConfig.Vhost),
		slog.String("username", conn.config.Username))
	mon.Event("rabbitmq_connected", conn.name)

	select {
	case <-ctx.Done():
		return true, errs2.IgnoreCanceled(c.Close())
	case <-conn.done:
		return true, ignoreClosed(c.Close())
	case reason := <-closed:
		if reason == nil {
			return true, Error.Errorf("connection closed")
		}
		return true, Error.Errorf("connection closed: %w", reason)
	}
}

// publish makes the connection visible to everyone waiting for one.
func (conn *Conn) publish(c *amqp.Connection) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	conn.conn = c
	conn.gen++
	close(conn.ready)
}

// retract withdraws it again and arms a fresh ready channel for the next
// connection. Close is never called here: the caller of runOnce owns the
// connection, so this cannot deadlock against a lock it already holds.
func (conn *Conn) retract() {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	conn.conn = nil
	conn.ready = make(chan struct{})
}

// Close stops the supervisor and closes the live connection. It is safe to
// call more than once, and safe to call before or during Run.
func (conn *Conn) Close() error {
	conn.closeOnce.Do(func() { close(conn.done) })

	conn.mu.Lock()
	c := conn.conn
	conn.mu.Unlock()

	if c == nil {
		return nil
	}
	return ignoreClosed(c.Close())
}

// Wait returns the live connection, blocking until there is one.
//
// It fails only when the caller's context ends or the Conn is closed. A broker
// that is down is something to wait for, which is what turns a redeploy of the
// broker into a pause rather than a wave of errors.
func (conn *Conn) Wait(ctx context.Context) (*amqp.Connection, error) {
	for {
		conn.mu.Lock()
		c, ready := conn.conn, conn.ready
		conn.mu.Unlock()

		if c != nil {
			return c, nil
		}

		select {
		case <-ready:
		case <-ctx.Done():
			return nil, Error.Wrap(ctx.Err())
		case <-conn.done:
			return nil, Error.Errorf("connection is closed")
		}
	}
}

// Channel opens a channel on the live connection.
//
// The channel does not outlive its connection: when the connection dies every
// channel on it dies with it, and nothing here re-opens this one. Use Session
// for anything that has to survive a reconnect.
func (conn *Conn) Channel(ctx context.Context) (*amqp.Channel, error) {
	c, err := conn.Wait(ctx)
	if err != nil {
		return nil, err
	}

	ch, err := c.Channel()
	if err != nil {
		return nil, Error.Wrap(err)
	}
	return ch, nil
}

// Session runs fn with a channel on the live connection, and runs it again on
// the next connection every time the current one dies.
//
// The context handed to fn is canceled when its channel dies, so a fn that
// blocks on deliveries or waits for a confirm learns about the disconnect the
// same way everything else does. An error fn returns after its connection died
// is a reconnect and is retried; any other error is the caller's own failure
// and is returned, because retrying a bug forever is worse than stopping.
//
// Publisher and Consumer are both this function with a body. It stays exported
// for the shapes they do not cover.
func (conn *Conn) Session(ctx context.Context, fn func(ctx context.Context, ch *amqp.Channel) error) (err error) {
	defer mon.Task()(&ctx)(&err)

	delay := conn.config.ReconnectDelay
	if delay <= 0 {
		delay = defaultReconnectDelay
	}

	for {
		if stopped(ctx, conn.done) {
			return nil
		}

		retry, err := conn.sessionOnce(ctx, fn)
		if !retry {
			return err
		}

		if err != nil {
			conn.log.Warn("rabbitmq session restarting",
				slog.String("name", conn.config.name()),
				slog.String("error", err.Error()))
			mon.Event("rabbitmq_session_restart", conn.name)
		}

		if !conn.sleep(ctx, delay) {
			return nil
		}
	}
}

// sleep waits for d and reports whether the wait finished.
//
// It watches conn.done as well as the context, because Close has to bring a
// supervisor down promptly even when it is mid-backoff. A shutdown that waits
// out a thirty-second reconnect delay is the kind of thing that turns a deploy
// into an incident.
func (conn *Conn) sleep(ctx context.Context, d time.Duration) bool {
	timer := time2.NewTimer(ctx, d)
	defer timer.Stop()

	select {
	case <-timer.Chan():
		return true
	case <-ctx.Done():
		return false
	case <-conn.done:
		return false
	}
}

// sessionOnce runs fn once against one channel. It reports whether the failure
// is one a new connection would fix.
func (conn *Conn) sessionOnce(ctx context.Context, fn func(ctx context.Context, ch *amqp.Channel) error) (retry bool, err error) {
	ch, err := conn.Channel(ctx)
	if err != nil {
		if stopped(ctx, conn.done) {
			return false, nil
		}
		// Wait only fails when the caller is done, so this is the connection
		// dying between Wait and Channel: try again on the next one.
		return true, err
	}
	defer func() { _ = ch.Close() }()

	// The channel's own death is what cancels fn's context. A channel error -
	// a PRECONDITION_FAILED, a nack that killed it - takes the channel down
	// without touching the connection, and fn has to hear about that too.
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case <-closed:
			cancel()
		case <-sessionCtx.Done():
		}
	}()
	defer func() { cancel(); <-watching }()

	err = fn(sessionCtx, ch)

	switch {
	case ctx.Err() != nil || stopped(ctx, conn.done):
		// The caller is shutting down, not the broker.
		return false, nil
	case err == nil:
		return false, nil
	case sessionCtx.Err() != nil || isBrokerError(err):
		return true, err
	default:
		return false, err
	}
}

// Connected reports whether there is a live connection right now. It is a
// snapshot: by the time it returns, it may be wrong.
func (conn *Conn) Connected() bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return conn.conn != nil
}

// Health opens a channel and closes it again.
//
// It does not check IsClosed. A TCP connection whose broker has stopped
// accepting channels is not usable and IsClosed does not say so, which is
// precisely what a readiness probe must not miss.
func (conn *Conn) Health(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	conn.mu.Lock()
	c := conn.conn
	conn.mu.Unlock()

	if c == nil {
		return Error.Errorf("not connected")
	}

	ch, err := c.Channel()
	if err != nil {
		return Error.Wrap(err)
	}
	return Error.Wrap(ch.Close())
}

// Stats implements monkit.StatSource.
//
// generation is the number of connections this Conn has made. It is the one
// number that says a broker is flapping: a generation climbing while
// connected stays 1 is a service that looks healthy on every scrape and is
// reconnecting between them.
func (conn *Conn) Stats(cb func(key monkit.SeriesKey, field string, val float64)) {
	key := monkit.NewSeriesKey("rabbitmq_conn").WithTags(conn.name)

	conn.mu.Lock()
	connected, gen := conn.conn != nil, conn.gen
	conn.mu.Unlock()

	cb(key, "connected", boolAsFloat(connected))
	cb(key, "generation", float64(gen))
}

// stopped reports whether the caller's context or the Conn is done.
func stopped(ctx context.Context, done chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	case <-done:
		return true
	default:
		return false
	}
}

// isBrokerError reports whether err is the broker or the library saying this
// channel or connection is gone, as opposed to the caller's own failure.
func isBrokerError(err error) bool {
	if errors.Is(err, amqp.ErrClosed) {
		return true
	}
	var amqpErr *amqp.Error
	return errors.As(err, &amqpErr)
}

// ignoreClosed drops the error from closing something that is already closed,
// which is the normal outcome of racing a shutdown against a disconnect.
func ignoreClosed(err error) error {
	if err == nil || errors.Is(err, amqp.ErrClosed) {
		return nil
	}
	return Error.Wrap(err)
}

func boolAsFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
