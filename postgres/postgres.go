// Package postgres constructs a configured, observable pgx connection pool.
//
// pgxpool.New does not connect. It returns a pool whose idle connections are
// created by a goroutine that discards its error, and MinConns defaults to
// zero, so with the default configuration nothing is dialed at all: a wrong
// host, an expired password or a firewall in the way produces a process that
// starts perfectly cleanly and reports itself healthy, and the failure surfaces
// on the first business query, attributed to whatever code happened to make it.
// Open pings, so a misconfigured database fails at startup next to the
// configuration that caused it.
//
// The second thing it prevents is a pool nobody can see into. The number that
// predicts a database outage is not query latency, it is how long a caller
// waited to be given a connection, and nothing measures that by default.
//
// # Session settings are sent once, at connect
//
// search_path, application_name and the statement timeouts go in the startup
// packet rather than in a SET per query. A SET on a pooled connection outlives
// the query that issued it and is inherited by the next borrower, which for
// search_path in a schema-per-tenant service is a cross-tenant data leak rather
// than a performance note. Sending them at connect also costs no round trip and
// makes every connection in the pool identical, so a failure cannot depend on
// which one you were given.
//
// # What this package deliberately does not do
//
// It wraps no query method and defines no interface. DB embeds *pgxpool.Pool,
// so every query, batch, copy and transaction pgx offers is reachable
// unchanged, and anything pgx grows later needs no work here. A caller who
// wants database/sql calls stdlib.OpenDBFromPool on the embedded pool.
//
// Migrations are out of scope. A migration runner would force a tool and a
// directory layout on every adopter, force a deploy-time decision about
// migrating at boot rather than as a job, and make this package own the
// distributed lock that migrating from every replica needs. A migration tool
// takes a DSN, which Config.ConnString already produces.
package postgres

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spacemonkeygo/monkit/v3"
)

// Defaults used when the config leaves a field at zero. They mirror pgxpool's
// own, so a reader does not have to open the library to know what zero means.
const (
	DefaultHealthTimeout     = 5 * time.Second
	DefaultMaxConnLifetime   = time.Hour
	DefaultMaxConnIdleTime   = 30 * time.Minute
	DefaultHealthCheckPeriod = time.Minute
)

// Config describes one Postgres connection pool.
//
// Two shapes are in use and both work: a DSN in URL, or the discrete
// Host/Port/Database/User/Password fields. URL wins when it is set, and the
// pool, timeout and session settings below are applied on top of it, so a DSN
// that says nothing about a pool size does not silently leave the library
// default in place.
type Config struct {
	Name string `help:"instance name; tags this pool's metrics so two pools can be told apart" default:"postgres"`

	URL      string `help:"postgres:// connection URL; when set it overrides host, port, database, user and password" secret:"optional"`
	Host     string `help:"database host, or an absolute path to a unix socket directory"                                               default:"127.0.0.1"`
	Port     int    `help:"database port"                                                                                               default:"5432"`
	Database string `help:"database name"                                                                                               default:"postgres"`
	User     string `help:"database user"                                                                                               default:"postgres"`
	Password string `help:"database password"                                                                         secret:"true"                         devDefault:"postgres" releaseDefault:""`

	SSLMode     string `help:"disable, allow, prefer, require, verify-ca or verify-full; prefer silently accepts plaintext"   devDefault:"prefer" releaseDefault:"require"`
	SSLRootCert string `help:"path to the CA certificate verify-ca and verify-full check against; empty uses the system pool"                                              default:"" path:"true"`

	ApplicationName string `help:"value reported in pg_stat_activity.application_name; empty uses the instance name" default:""`
	SearchPath      string `help:"schema search path, set once per connection; empty uses the server's"              default:""`

	MaxConns              int           `help:"maximum connections in the pool; 0 uses the greater of 4 and the CPU count"                      default:"0"`
	MinConns              int           `help:"connections kept open while idle; 0 leaves the pool empty until first use"                       default:"0"`
	MaxConnLifetime       time.Duration `help:"how long a connection may live before it is retired; 0 uses 1h"                                  default:"0"`
	MaxConnLifetimeJitter time.Duration `help:"random amount subtracted from each connection's lifetime, so a pool does not retire all at once" default:"0"`
	MaxConnIdleTime       time.Duration `help:"how long an unused connection is kept before it is closed; 0 uses 30m"                           default:"0"`
	HealthCheckPeriod     time.Duration `help:"how often the pool checks for expired and missing connections; 0 uses 1m"                        default:"0"`

	ConnectTimeout                  time.Duration `help:"timeout for establishing one connection"                                                   default:"5s"`
	StatementTimeout                time.Duration `help:"server-side cancellation of a statement that runs too long; 0 uses the server's setting"   default:"0"`
	IdleInTransactionSessionTimeout time.Duration `help:"server-side cancellation of a transaction left open and idle; 0 uses the server's setting" default:"60s"`

	RequireOnStart bool          `help:"refuse to start if the database cannot be reached" default:"true"`
	HealthTimeout  time.Duration `help:"bound on the startup ping and on Health"           default:"5s"`

	Metrics bool `help:"record monkit series for queries and the pool" default:"true"`

	// Query logging. Arguments are never logged: pgx sends them separately, so
	// the statement is the parameterized form and the values that identify a
	// person stay out of the log.
	LogQueries bool          `help:"log every statement at debug level; bind parameters are never logged"          default:"false"`
	SlowQuery  time.Duration `help:"log any statement taking at least this long at warn level; 0 disables it"      default:"0"`
}

// Validate reports settings that bind and parse but cannot work.
func (config Config) Validate() error {
	switch config.SSLMode {
	case "",
		"disable",
		"allow",
		"prefer",
		"require",
		"verify-ca",
		"verify-full":
	default:
		return Error.Errorf(
			"sslmode %q is not a postgres sslmode",
			config.SSLMode,
		)
	}

	if config.URL == "" && config.Host == "" {
		return Error.Errorf("host is empty")
	}
	if config.MaxConns < 0 || config.MinConns < 0 {
		return Error.Errorf("connection counts must not be negative")
	}
	if config.MinConns > 0 && config.MaxConns > 0 &&
		config.MinConns > config.MaxConns {
		return Error.Errorf(
			"min-conns %d is greater than max-conns %d",
			config.MinConns,
			config.MaxConns,
		)
	}
	return nil
}

// ConnString returns the DSN this config dials.
//
// It is not named URL because that is what an operator sets; this is what the
// package derives from it. The result contains the password, so it must never
// be logged - which is why the logging in this package names the host, port,
// database and user instead.
//
// When the discrete fields are used the result is a keyword/value string rather
// than a URL. That form needs no percent-escaping decisions about a password
// containing an @ or a /, and it can express host=/var/run/postgresql, a unix
// socket directory, which a URL host cannot.
func (config Config) ConnString() string {
	if config.URL != "" {
		// Returned whole. Merging flag defaults into an operator's DSN would
		// mean an sslmode=verify-full in the DSN could be overridden by a flag
		// nobody set.
		return config.URL
	}

	pairs := [][2]string{
		{"host", config.Host},
		{"port", strconv.Itoa(config.Port)},
		{"dbname", config.Database},
		{"user", config.User},
	}
	if config.Password != "" {
		pairs = append(pairs, [2]string{"password", config.Password})
	}
	if config.SSLMode != "" {
		pairs = append(pairs, [2]string{"sslmode", config.SSLMode})
	}
	if config.SSLRootCert != "" {
		pairs = append(pairs, [2]string{"sslrootcert", config.SSLRootCert})
	}

	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, pair[0]+"="+quoteValue(pair[1]))
	}
	return strings.Join(parts, " ")
}

// quoteValue escapes a keyword/value element the way libpq expects.
func quoteValue(value string) string {
	if value != "" && !strings.ContainsAny(value, " '\\") {
		return value
	}
	replacer := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + replacer.Replace(value) + "'"
}

// PoolConfig translates the Config into a pgxpool config.
//
// It is exported so a service with an unusual requirement can start from the
// configured pool and adjust one field rather than abandoning the Config
// entirely. The result is what OpenWith takes.
func (config Config) PoolConfig() (*pgxpool.Config, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	poolConfig, err := pgxpool.ParseConfig(config.ConnString())
	if err != nil {
		// The DSN carries the password, and a logged DSN is a leaked
		// credential.
		return nil, Error.Errorf("parsing postgres dsn: %w", redactDSN(err))
	}

	// Applied after parsing on purpose. A DSN that says nothing about a pool
	// size would otherwise leave the library default in place, and a DSN that
	// does say something keeps saying it when the flag is unset.
	if config.MaxConns > 0 {
		poolConfig.MaxConns = int32(
			config.MaxConns,
		) //nolint:gosec // bounded by Validate
	}
	if config.MinConns > 0 {
		poolConfig.MinConns = int32(
			config.MinConns,
		) //nolint:gosec // bounded by Validate
	}
	if config.MaxConnLifetime > 0 {
		poolConfig.MaxConnLifetime = config.MaxConnLifetime
	}
	if config.MaxConnLifetimeJitter > 0 {
		poolConfig.MaxConnLifetimeJitter = config.MaxConnLifetimeJitter
	}
	if config.MaxConnIdleTime > 0 {
		poolConfig.MaxConnIdleTime = config.MaxConnIdleTime
	}
	if config.HealthCheckPeriod > 0 {
		poolConfig.HealthCheckPeriod = config.HealthCheckPeriod
	}
	if config.ConnectTimeout > 0 {
		poolConfig.ConnConfig.ConnectTimeout = config.ConnectTimeout
	}

	config.applySessionSettings(poolConfig)
	return poolConfig, nil
}

// applySessionSettings puts the session parameters in the startup packet.
func (config Config) applySessionSettings(poolConfig *pgxpool.Config) {
	params := poolConfig.ConnConfig.RuntimeParams
	if params == nil {
		params = map[string]string{}
		poolConfig.ConnConfig.RuntimeParams = params
	}

	applicationName := config.ApplicationName
	if applicationName == "" {
		applicationName = config.name()
	}
	params["application_name"] = applicationName

	if config.SearchPath != "" {
		params["search_path"] = config.SearchPath
	}
	if config.StatementTimeout > 0 {
		params["statement_timeout"] = milliseconds(config.StatementTimeout)
	}
	if config.IdleInTransactionSessionTimeout > 0 {
		params["idle_in_transaction_session_timeout"] = milliseconds(
			config.IdleInTransactionSessionTimeout,
		)
	}
}

func milliseconds(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

func (config Config) name() string {
	if config.Name == "" {
		return "postgres"
	}
	return config.Name
}

// DB is a configured Postgres connection pool.
//
// The embedded *pgxpool.Pool is the API: this type adds construction, a health
// check and metrics, and deliberately wraps no query method.
type DB struct {
	*pgxpool.Pool

	log    *slog.Logger
	config Config
	name   monkit.SeriesTag
}

// Open connects to Postgres, pings it, and returns a pool.
//
// The ping is the point. pgxpool.New returns a pool that has not connected to
// anything, so without it a wrong host or an expired password starts cleanly
// and fails on the first business query. With RequireOnStart that failure
// happens here, next to the configuration that caused it; without it the
// failure is logged and the pool is returned, for a database whose absence at
// startup is survivable.
//
// The returned DB must be closed. It is a lifecycle.Item with a Close and no
// Run.
func Open(ctx context.Context, log *slog.Logger, config Config) (*DB, error) {
	poolConfig, err := config.PoolConfig()
	if err != nil {
		return nil, err
	}
	return OpenWith(ctx, log, poolConfig, config)
}

// OpenWith is Open over a pool config the caller built.
//
// The config must have come from pgxpool.ParseConfig - pgxpool panics on one
// that did not, and a panic out of a shared constructor is not acceptable, so
// this checks what it can and reports instead.
//
// The context is held by the pool for the life of its background connection
// priming, so pass the service's context rather than a short-lived startup one.
// It is not what proves the database is reachable; the ping below is.
func OpenWith(
	ctx context.Context,
	log *slog.Logger,
	poolConfig *pgxpool.Config,
	config Config,
) (db *DB, err error) {
	defer mon.Task()(&ctx)(&err)

	if log == nil {
		return nil, Error.Errorf("logger is nil")
	}
	if poolConfig == nil || poolConfig.ConnConfig == nil {
		return nil, Error.Errorf(
			"pool config must come from pgxpool.ParseConfig",
		)
	}

	nameTag := monkit.NewSeriesTag("name", config.name())

	if config.Metrics || config.wantsQueryLog() {
		// The assertions pgx makes to find the query, batch, connect and
		// acquire hooks run inside NewWithConfig, so the tracer has to be in
		// place before the pool is built - unlike go-redis, where the hook is
		// added afterwards.
		if poolConfig.ConnConfig.Tracer == nil {
			poolConfig.ConnConfig.Tracer = newTracer(log, nameTag, config)
		} else {
			// Replacing a caller's tracer would silently delete their tracing.
			log.Warn(
				"a pgx tracer is already installed; "+
					"postgres metrics and query logging are off for this pool",
				"name",
				config.name(),
			)
		}
	}

	log.Info("connecting to postgres",
		"name", config.name(),
		"host", poolConfig.ConnConfig.Host,
		"port", poolConfig.ConnConfig.Port,
		"database", poolConfig.ConnConfig.Database,
		"user", poolConfig.ConnConfig.User)

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, Error.Wrap(redactDSN(err))
	}

	db = &DB{Pool: pool, log: log, config: config, name: nameTag}

	if err := db.Health(ctx); err != nil {
		if config.RequireOnStart {
			pool.Close()
			return nil, err
		}
		log.Warn(
			"postgres is not reachable; continuing because require-on-start is off",
			"name",
			config.name(),
			"err",
			err.Error(),
		)
	}

	return db, nil
}

// Health pings the database, bounded by HealthTimeout.
//
// It is not called Ping, even though the signature would allow shadowing the
// embedded one, because a Ping that quietly added a timeout the caller did not
// ask for would be worse than one that does not compile. The raw command stays
// reachable as db.Pool.Ping.
//
// Pool.Ping acquires a connection and runs an empty statement, which is what a
// readiness probe wants: it exercises acquisition and a round trip rather than
// just a socket.
func (db *DB) Health(ctx context.Context) error {
	timeout := db.config.HealthTimeout
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return Error.Wrap(db.Ping(ctx))
}

// Close closes the pool.
//
// It returns an error only so the type fits lifecycle.Item: pgxpool.Pool.Close
// returns nothing and is guarded by a sync.Once, so calling this twice is safe.
func (db *DB) Close() error {
	db.Pool.Close()
	return nil
}

// Stats implements monkit.StatSource over the pool's own counters.
//
// It is not registered here. Scope.Chain appends unconditionally, so a
// constructor that registered itself would double-count every pool a test
// opened. The caller chains it once, at the composition root:
//
//	monkit.Default.ScopeNamed("postgres").Chain(db)
func (db *DB) Stats(cb func(key monkit.SeriesKey, field string, val float64)) {
	key := monkit.NewSeriesKey("postgres_pool").
		WithTag(db.name.Key, db.name.Val)
	stat := db.Stat()

	// All thirteen of pgxpool.Stat's counters, because they are read from one
	// snapshot and none of them costs anything. The ones that matter most:
	// empty_acquire_count is the best "max-conns is too low" signal there is,
	// and canceled_acquire_count is a saturated pool shedding load, which
	// otherwise shows up only as request timeouts a long way from the cause.
	cb(key, "acquire_count", float64(stat.AcquireCount()))
	cb(key, "acquire_duration_seconds", stat.AcquireDuration().Seconds())
	cb(key, "acquired_conns", float64(stat.AcquiredConns()))
	cb(key, "canceled_acquire_count", float64(stat.CanceledAcquireCount()))
	cb(key, "constructing_conns", float64(stat.ConstructingConns()))
	cb(key, "empty_acquire_count", float64(stat.EmptyAcquireCount()))
	cb(key, "idle_conns", float64(stat.IdleConns()))
	cb(key, "max_conns", float64(stat.MaxConns()))
	cb(key, "total_conns", float64(stat.TotalConns()))
	cb(key, "new_conns_count", float64(stat.NewConnsCount()))
	cb(
		key,
		"max_lifetime_destroy_count",
		float64(stat.MaxLifetimeDestroyCount()),
	)
	cb(key, "max_idle_destroy_count", float64(stat.MaxIdleDestroyCount()))
	cb(key, "empty_acquire_wait_seconds", stat.EmptyAcquireWaitTime().Seconds())
}

// redactDSN removes a connection string from an error before it is logged.
//
// pgx already keeps the password out of ParseConfigError.Error, but
// ParseConfigError.ConnString is an exported field holding the raw DSN: the
// error string is safe while %+v, slog.Any and a JSON handler are not, and a
// logged DSN is a leaked credential.
func redactDSN(err error) error {
	var parseErr *pgconn.ParseConfigError
	if errors.As(err, &parseErr) {
		parseErr.ConnString = "[redacted]"
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = "[redacted]"
	}
	return err
}
