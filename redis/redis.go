// Package redis constructs a configured, observable go-redis client.
//
// go-redis dials lazily. A service with a typo in its address, an expired
// password or a firewall in the way starts perfectly cleanly and reports
// itself healthy; the failure surfaces on the first business request, hours
// later, attributed to whatever code happened to make it. Open dials and
// pings, so a misconfigured cache fails at startup next to the configuration
// that caused it.
//
// The second thing it prevents is a silently unobservable client. The pool
// carries a monkit hook, so command latency and error rates exist for every
// service without anyone remembering to add them, and the hook knows
// redis.Nil is a cache miss rather than an error, which is the one thing a
// hand-rolled counter always gets wrong.
//
// # What this package deliberately does not do
//
// It wraps no commands. Client embeds *redis.Client, so every command, every
// pipeline and every future addition to the library is reachable unchanged.
// There is no Cache interface here: the moment there is one, every feature
// go-redis grows has to be re-exported through it, and callers start choosing
// between the abstraction and the thing it abstracts.
//
// Reconnection, pooling and command retry are go-redis's own and are left to
// it. This package adds construction, a startup ping, timeouts, metrics and
// Close - nothing above the client.
//
// # Both packages are called redis
//
// Import the library under an alias, which is what this package does
// internally:
//
//	import (
//		goredis "github.com/redis/go-redis/v9"
//		"gitlab.internal/tuan.quang.tran/aioz-common/redis"
//	)
//
// A caller needs the alias to reach goredis.Nil, which is the sentinel a cache
// miss returns.
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/url"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spacemonkeygo/monkit/v3"
)

// Config describes one redis connection pool.
//
// Two shapes are in use across our services and both work here: a DSN in URL,
// or the discrete Address/DB/Username/Password fields. URL wins when it is
// set, and the pool, timeout and retry fields below are applied on top of it,
// so a DSN never silently reverts a timeout to the library default.
type Config struct {
	Name string `help:"instance name; tags this client's metrics so two pools can be told apart" default:"redis"`

	URL      string `help:"redis connection URL (redis:// or rediss://); when set it overrides address, db, username and password" secret:"optional"`
	Address  string `help:"redis server address as host:port" default:"127.0.0.1:6379"`
	DB       int    `help:"logical database number" default:"0"`
	Username string `help:"ACL username; empty uses the default user" default:""`
	Password string `help:"redis password; empty means no AUTH" secret:"optional"`

	ClientName string `help:"name this connection reports to CLIENT LIST" default:""`

	PoolSize     int `help:"maximum socket connections; 0 uses 10 per CPU" default:"0"`
	MinIdleConns int `help:"connections kept open while idle" default:"0"`
	MaxRetries   int `help:"retries per command before giving up; -1 disables retrying" default:"3"`

	DialTimeout  time.Duration `help:"timeout establishing a connection" default:"5s"`
	ReadTimeout  time.Duration `help:"socket read timeout; -1 disables" default:"3s"`
	WriteTimeout time.Duration `help:"socket write timeout; -1 disables" default:"3s"`
	PoolTimeout  time.Duration `help:"how long a caller waits for a free connection; 0 uses ReadTimeout+1s" default:"0"`

	TLS           bool `help:"connect with TLS" default:"false"`
	TLSSkipVerify bool `help:"do not verify the server certificate; test use only" default:"false" hidden:"true"`

	RequireOnStart bool          `help:"refuse to start if the server cannot be reached" default:"true"`
	HealthTimeout  time.Duration `help:"bound on the startup ping and on Health" default:"3s"`

	Metrics bool `help:"record monkit series for commands and the pool" default:"true"`
}

// DefaultHealthTimeout bounds the startup ping and Health when the config
// leaves HealthTimeout at zero.
const DefaultHealthTimeout = 3 * time.Second

// Options translates the Config into go-redis options.
//
// It is exported so a service with an unusual requirement can start from the
// configured pool and adjust one field rather than abandoning the Config
// entirely.
func (config Config) Options() (*goredis.Options, error) {
	var opts *goredis.Options

	if config.URL != "" {
		parsed, err := goredis.ParseURL(config.URL)
		if err != nil {
			// The URL carries the password, so it must not reach the error
			// string: an error is logged, and a logged DSN is a leaked
			// credential.
			return nil, Error.Errorf("parsing redis url: %w", redactURLError(err))
		}
		opts = parsed
	} else {
		if config.Address == "" {
			return nil, Error.Errorf("address is empty")
		}
		opts = &goredis.Options{
			Addr:     config.Address,
			DB:       config.DB,
			Username: config.Username,
			Password: config.Password,
		}
	}

	if config.ClientName != "" {
		opts.ClientName = config.ClientName
	}

	// Applied after ParseURL on purpose. A DSN that says nothing about
	// timeouts would otherwise leave the library defaults in place, and the
	// whole reason these fields exist is that no service in the estate sets
	// one today.
	opts.PoolSize = config.PoolSize
	opts.MinIdleConns = config.MinIdleConns
	opts.MaxRetries = config.MaxRetries
	opts.DialTimeout = config.DialTimeout
	opts.ReadTimeout = config.ReadTimeout
	opts.WriteTimeout = config.WriteTimeout
	opts.PoolTimeout = config.PoolTimeout

	if config.TLS && opts.TLSConfig == nil {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: hostOf(opts.Addr),
		}
	}
	if config.TLSSkipVerify && opts.TLSConfig != nil {
		opts.TLSConfig.InsecureSkipVerify = true
	}

	return opts, nil
}

// Client is a configured redis connection pool.
//
// The embedded *redis.Client is the API: this type adds construction, a
// health check and metrics, and deliberately wraps no commands.
type Client struct {
	*goredis.Client

	log    *slog.Logger
	config Config
	name   monkit.SeriesTag
}

// Open dials redis, pings it, and returns a client.
//
// The ping is the point. With RequireOnStart a server that cannot be reached
// fails here, next to the configuration that caused it, rather than on the
// first business request. Without it the failure is logged and the client is
// returned, for a cache whose absence is survivable.
//
// The returned client must be closed. It is a lifecycle.Item with a Close and
// no Run.
func Open(ctx context.Context, log *slog.Logger, config Config) (*Client, error) {
	opts, err := config.Options()
	if err != nil {
		return nil, err
	}
	return OpenWith(ctx, log, opts, config)
}

// OpenWith is Open for a caller who already has go-redis options.
//
// It is the escape hatch for an endpoint that arrives in a shape ParseURL does
// not cover: the options describe the connection, and config supplies only the
// name, health and metrics settings.
func OpenWith(ctx context.Context, log *slog.Logger, opts *goredis.Options, config Config) (client *Client, err error) {
	defer mon.Task()(&ctx)(&err)

	if opts == nil {
		return nil, Error.Errorf("options are nil")
	}
	if log == nil {
		return nil, Error.Errorf("logger is nil")
	}

	name := config.Name
	if name == "" {
		name = "redis"
	}

	client = &Client{
		Client: goredis.NewClient(opts),
		log:    log,
		config: config,
		name:   monkit.NewSeriesTag("name", name),
	}

	if config.Metrics {
		client.AddHook(newHook(client.name))
	}

	// Never log opts.Password, and never log a DSN. Address, DB and username
	// are what identifies the server; the credential is not.
	log.Info("connecting to redis",
		slog.String("name", name),
		slog.String("address", opts.Addr),
		slog.Int("db", opts.DB))

	if err := client.Health(ctx); err != nil {
		if config.RequireOnStart {
			_ = client.Close()
			return nil, err
		}
		log.Warn("redis is not reachable; continuing because require-on-start is off",
			slog.String("name", name),
			slog.String("address", opts.Addr),
			slog.String("error", err.Error()))
	}

	return client, nil
}

// Health pings the server, bounded by HealthTimeout.
//
// It is called Health rather than Ping on purpose: a Ping(ctx) error would
// shadow the embedded Ping(ctx) *StatusCmd with a different return type, which
// is a trap for anyone reading a call site. The raw command stays reachable as
// client.Client.Ping(ctx).
func (client *Client) Health(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	timeout := client.config.HealthTimeout
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := client.Client.Ping(ctx).Err(); err != nil {
		return Error.Wrap(err)
	}
	return nil
}

// Close releases the pool. It is safe to call more than once.
func (client *Client) Close() error {
	return Error.Wrap(client.Client.Close())
}

// Stats implements monkit.StatSource over the connection pool.
//
// Open does not chain it: Scope.Chain appends unconditionally, so a
// constructor that registered would double-count every pool a test opened.
// The service does it once, in its composition root:
//
//	monkit.Default.ScopeNamed("redis").Chain(client)
func (client *Client) Stats(cb func(key monkit.SeriesKey, field string, val float64)) {
	key := monkit.NewSeriesKey("redis_pool").WithTags(client.name)
	stats := client.PoolStats()

	cb(key, "hits", float64(stats.Hits))
	cb(key, "misses", float64(stats.Misses))
	cb(key, "timeouts", float64(stats.Timeouts))
	cb(key, "total_conns", float64(stats.TotalConns))
	cb(key, "idle_conns", float64(stats.IdleConns))
	cb(key, "stale_conns", float64(stats.StaleConns))
}

// redactURLError removes the DSN from a url.Error before it reaches a log
// line. url.Parse puts the string it failed on into the error, and for a redis
// DSN that string contains the password.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = "[redacted]"
	}
	return err
}

// hostOf returns the host part of a host:port address, for a TLS ServerName.
func hostOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
