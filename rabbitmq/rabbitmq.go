// Package rabbitmq is the AMQP client for our services: a supervised
// connection, a declared topology, a publisher and a consumer.
//
// It goes further than a connection wrapper on purpose. Eight hand-rolled
// wrappers exist across our repos, three of them stale forks of each other,
// and supervising the connection alone would let each of them delete its dial
// block and keep two hundred lines of publisher and consumer. So this package
// owns everything up to the message, which is exactly the set of features that
// was written independently more than once: reconnect, topology re-declared on
// reconnect, JSON publishing with confirms, prefetch, a bounded worker pool,
// manual ack with a dead-letter path, and a graceful drain.
//
// # Four layers, each usable alone
//
// Conn supervises one connection and redials forever. Topology declares
// exchanges, queues and bindings, and is re-declared on every reconnect
// because a new connection inherits none of the previous one's channel state.
// Publisher sends on a confirming channel. Consumer receives into a bounded
// pool and acks by handler outcome.
//
// A service that needs a shape none of them express drops to Conn.Session and
// a raw *amqp.Channel, which is what the escape hatch is for.
//
// # The bugs this design excludes
//
// Publisher confirms are correlated per publish through a
// DeferredConfirmation, so concurrent publishers cannot consume each other's
// acks - a live bug in one of the wrappers this replaces, where a failed
// publish could report success.
//
// The channel a reconnect watches is a local variable per attempt, never a
// struct field, so a second reconnect cannot end up selecting on a dead
// channel forever. Close never runs under a lock its caller already holds.
// Backoff is exponential and bounded, and it never gives up: a broker down for
// ten minutes is an outage to ride out, not a reason to exit.
//
// Messages are persistent by default, consumed with manual ack, and drained at
// shutdown, so a restart does not lose what was in flight.
package rabbitmq

import (
	"crypto/tls"
	"errors"
	"net/url"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Config describes one broker connection.
//
// URL and the discrete fields are alternatives: when URL is set it is used
// whole, which is how a platform-supplied DSN is passed through. Otherwise the
// address, vhost and credentials build one.
type Config struct {
	Name string `help:"connection name shown in the broker UI and used to tag metrics" default:"aioz"`

	URL      string `help:"amqp:// or amqps:// URL; when set it overrides address, vhost, username and password" secret:"optional"`
	Address  string `help:"broker address as host:port" default:"127.0.0.1:5672"`
	Vhost    string `help:"virtual host" default:"/"`
	Username string `help:"broker username" default:"guest"`
	Password string `help:"broker password" secret:"optional" devDefault:"guest" releaseDefault:""`

	TLS           bool `help:"connect with TLS (amqps)" default:"false"`
	TLSSkipVerify bool `help:"do not verify the broker certificate; test use only" default:"false" hidden:"true"`

	DialTimeout time.Duration `help:"timeout for one connection attempt" default:"10s"`
	Heartbeat   time.Duration `help:"heartbeat interval; 0 uses the broker's" default:"10s"`
	ChannelMax  int           `help:"maximum channels on the connection; 0 means the protocol maximum" default:"0"`
	FrameSize   int           `help:"maximum frame size in bytes; 0 means unlimited" default:"0"`

	ReconnectDelay    time.Duration `help:"wait after a failed attempt before retrying" default:"1s"`
	ReconnectMaxDelay time.Duration `help:"upper bound on the exponential reconnect backoff" default:"30s"`

	RequireOnStart bool `help:"fail startup if the first connection attempt fails, instead of retrying in the background" default:"false"`
}

// Default backoff bounds, used when the config leaves them at zero.
const (
	defaultReconnectDelay    = time.Second
	defaultReconnectMaxDelay = 30 * time.Second
)

// DialURL builds the amqp:// or amqps:// URL this config dials.
//
// It is not called URL because the config field of that name is what an
// operator sets; this is what the package derives from it.
//
// The result contains the password: never log it. Log Address, Vhost and
// Username instead, which is what this package does.
func (config Config) DialURL() string {
	if config.URL != "" {
		return config.URL
	}

	scheme := "amqp"
	if config.TLS {
		scheme = "amqps"
	}

	vhost := config.Vhost
	if vhost == "" {
		vhost = "/"
	}

	built := url.URL{
		Scheme: scheme,
		Host:   config.Address,
		// A vhost is a path segment, so "/" is the empty path and any other
		// name is escaped. url.URL does that escaping through Path.
		Path: vhost,
	}
	if config.Username != "" || config.Password != "" {
		built.User = url.UserPassword(config.Username, config.Password)
	}

	return built.String()
}

// amqp builds the dial config.
//
// DialTimeout is applied through Dial rather than a context: amqp091 has no
// DialContext, only Dial and DialConfig, so the timeout has to ride on the
// dialer itself.
func (config Config) amqp() (amqp.Config, error) {
	if config.ChannelMax < 0 || config.ChannelMax > 65535 {
		return amqp.Config{}, Error.Errorf(
			"channel-max must be between 0 and 65535, got %d", config.ChannelMax)
	}
	if config.FrameSize < 0 {
		return amqp.Config{}, Error.Errorf(
			"frame-size must not be negative, got %d", config.FrameSize)
	}

	dialTimeout := config.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second
	}

	out := amqp.Config{
		Vhost:      config.Vhost,
		ChannelMax: uint16(config.ChannelMax),
		FrameSize:  config.FrameSize,
		Heartbeat:  config.Heartbeat,
		Locale:     "en_US",
		Dial:       amqp.DefaultDial(dialTimeout),
		Properties: amqp.Table{
			// The broker's connection list shows this, which is the difference
			// between "some Go client" and a named service when something has
			// to be closed by hand at 3am.
			"connection_name": config.name(),
			"product":         "aioz-common",
		},
	}

	// DialConfig takes the vhost from the config, not the URL, so a URL-only
	// configuration has to have it read back out.
	if config.URL != "" {
		parsed, err := url.Parse(config.URL)
		if err != nil {
			return amqp.Config{}, Error.Errorf("parsing amqp url: %w", redactURLError(err))
		}
		out.Vhost = vhostOf(parsed)
	}
	if out.Vhost == "" {
		out.Vhost = "/"
	}

	if config.TLS || isTLSURL(config.URL) {
		out.TLSClientConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: config.TLSSkipVerify, //nolint:gosec // operator opt-in, hidden flag, test use only
		}
	}

	return out, nil
}

func (config Config) name() string {
	if config.Name == "" {
		return "aioz"
	}
	return config.Name
}

// vhostOf reads the vhost out of a parsed amqp URL. The path is the vhost, and
// an empty path means the default "/".
func vhostOf(parsed *url.URL) string {
	if parsed.Path == "" || parsed.Path == "/" {
		return "/"
	}
	return parsed.Path[1:]
}

func isTLSURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "amqps"
}

// redactURLError removes the DSN from a url.Error before it reaches a log
// line. url.Parse puts the string it failed on into the error, and an amqp URL
// contains the password.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = "[redacted]"
	}
	return err
}
