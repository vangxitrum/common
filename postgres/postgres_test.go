package postgres_test

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/postgres"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

// testLogger routes the package's log lines into the test output, so a failure
// shows what the pool was doing.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(
		slog.NewTextHandler(
			&testWriter{t: t},
			&slog.HandlerOptions{Level: slog.LevelDebug},
		),
	)
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p))
	return len(p), nil
}

// baseConfig is what config.Bind produces from the struct tags, which is what
// an operator actually gets.
func baseConfig(t *testing.T) postgres.Config {
	t.Helper()
	return postgres.Config{
		Name:                            t.Name(),
		Host:                            "127.0.0.1",
		Port:                            5432,
		Database:                        "postgres",
		User:                            "postgres",
		Password:                        "postgres",
		SSLMode:                         "prefer",
		ConnectTimeout:                  5 * time.Second,
		IdleInTransactionSessionTimeout: time.Minute,
		RequireOnStart:                  true,
		HealthTimeout:                   5 * time.Second,
		Metrics:                         true,
	}
}

// The assertion is a round trip rather than string equality: equality would
// test the formatter, a round trip tests the contract.
func TestConnStringRoundTrips(t *testing.T) {
	t.Parallel()

	for name, password := range map[string]string{
		"a plain password":            "hunter2",
		"a password with a space":     "hunter 2",
		"a password with a quote":     "hun'ter2",
		"a password with a backslash": `hun\ter2`,
		"a password with an at sign":  "hun@ter/2",
		"no password":                 "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			config := baseConfig(t)
			config.Password = password
			config.Database = "billing"

			parsed, err := pgxpool.ParseConfig(config.ConnString())
			require.NoError(t, err)

			require.Equal(t, "127.0.0.1", parsed.ConnConfig.Host)
			require.Equal(t, uint16(5432), parsed.ConnConfig.Port)
			require.Equal(t, "billing", parsed.ConnConfig.Database)
			require.Equal(t, "postgres", parsed.ConnConfig.User)
			require.Equal(t, password, parsed.ConnConfig.Password)
		})
	}
}

// A unix socket directory is not expressible as a URL host, which is why the
// discrete fields produce a keyword/value string.
func TestConnStringSupportsAUnixSocket(t *testing.T) {
	t.Parallel()

	config := baseConfig(t)
	config.Host = "/var/run/postgresql"

	parsed, err := pgxpool.ParseConfig(config.ConnString())
	require.NoError(t, err)
	require.Equal(t, "/var/run/postgresql", parsed.ConnConfig.Host)
}

func TestURLOverridesTheDiscreteFields(t *testing.T) {
	t.Parallel()

	config := baseConfig(t)
	config.URL = "postgres://someone:secret@db.internal:6543/ledger?sslmode=verify-full"

	parsed, err := pgxpool.ParseConfig(config.ConnString())
	require.NoError(t, err)

	require.Equal(t, "db.internal", parsed.ConnConfig.Host)
	require.Equal(t, uint16(6543), parsed.ConnConfig.Port)
	require.Equal(t, "ledger", parsed.ConnConfig.Database)
	require.Equal(t, "someone", parsed.ConnConfig.User)

	// sslmode is part of where and how you connect, which is what a DSN is, so
	// a flag default must not override what the DSN said.
	require.NotNil(t, parsed.ConnConfig.TLSConfig)
	require.False(t, parsed.ConnConfig.TLSConfig.InsecureSkipVerify)
}

// A DSN that says nothing about a pool must not leave the library default in
// place, and one that does say something must keep saying it.
func TestPoolSizingAppliesOverAParsedURL(t *testing.T) {
	t.Parallel()

	t.Run("the flags fill a silent DSN", func(t *testing.T) {
		t.Parallel()

		config := baseConfig(t)
		config.URL = "postgres://someone:secret@db.internal:5432/ledger"
		config.MaxConns = 7
		config.MaxConnLifetime = 11 * time.Minute
		config.HealthCheckPeriod = 13 * time.Second

		poolConfig, err := config.PoolConfig()
		require.NoError(t, err)

		require.Equal(t, int32(7), poolConfig.MaxConns)
		require.Equal(t, 11*time.Minute, poolConfig.MaxConnLifetime)
		require.Equal(t, 13*time.Second, poolConfig.HealthCheckPeriod)
	})

	t.Run("an unset flag leaves the DSN's own value", func(t *testing.T) {
		t.Parallel()

		config := baseConfig(t)
		config.URL = "postgres://someone:secret@db.internal:5432/ledger?pool_max_conns=9"

		poolConfig, err := config.PoolConfig()
		require.NoError(t, err)
		require.Equal(t, int32(9), poolConfig.MaxConns)
	})
}

// sslmode spans the whole TLS ladder, which is why there is no TLSSkipVerify
// field - and why mutating TLSConfig after parsing would be wrong: prefer and
// allow are implemented as fallbacks, each carrying its own TLS config.
//
// Note what prefer actually is: an unverified TLS attempt with a plaintext
// fallback. It is not encryption in any sense worth relying on, which is why
// the release default is require.
func TestSSLMode(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mode          string
		wantTLS       bool
		wantFallbacks bool
		wantSkip      bool
		wantErr       bool
	}{
		"disable is plaintext":            {mode: "disable"},
		"prefer falls back to plaintext":  {mode: "prefer", wantTLS: true, wantFallbacks: true, wantSkip: true},
		"require encrypts without naming": {mode: "require", wantTLS: true, wantSkip: true},
		"verify-full checks the name":     {mode: "verify-full", wantTLS: true},
		"anything else is a mistake":      {mode: "sure-why-not", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			config := baseConfig(t)
			config.SSLMode = tc.mode

			poolConfig, err := config.PoolConfig()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			if !tc.wantTLS {
				require.Nil(t, poolConfig.ConnConfig.TLSConfig)
				return
			}

			require.NotNil(t, poolConfig.ConnConfig.TLSConfig)
			require.Equal(
				t,
				tc.wantSkip,
				poolConfig.ConnConfig.TLSConfig.InsecureSkipVerify,
			)

			if tc.wantFallbacks {
				// The reason a post-parse TLSConfig assignment would be a bug:
				// it would configure this attempt and leave the plaintext
				// fallback alone.
				require.NotEmpty(t, poolConfig.ConnConfig.Fallbacks)
			}
		})
	}
}

func TestSessionSettingsLandInTheStartupPacket(t *testing.T) {
	t.Parallel()

	config := baseConfig(t)
	config.SearchPath = "tenant_7, public"
	config.StatementTimeout = 3 * time.Second
	config.IdleInTransactionSessionTimeout = 90 * time.Second

	poolConfig, err := config.PoolConfig()
	require.NoError(t, err)

	params := poolConfig.ConnConfig.RuntimeParams
	require.Equal(
		t,
		t.Name(),
		params["application_name"],
		"it falls back to the instance name",
	)
	require.Equal(t, "tenant_7, public", params["search_path"])
	require.Equal(t, "3000", params["statement_timeout"])
	require.Equal(t, "90000", params["idle_in_transaction_session_timeout"])
}

// A zero duration must omit the parameter, not send "0", which to Postgres
// means "no limit" rather than "use the server's setting".
func TestZeroTimeoutsAreOmitted(t *testing.T) {
	t.Parallel()

	config := baseConfig(t)
	config.StatementTimeout = 0
	config.IdleInTransactionSessionTimeout = 0
	config.ApplicationName = "explicit"

	poolConfig, err := config.PoolConfig()
	require.NoError(t, err)

	params := poolConfig.ConnConfig.RuntimeParams
	require.NotContains(t, params, "statement_timeout")
	require.NotContains(t, params, "idle_in_transaction_session_timeout")
	require.NotContains(t, params, "search_path")
	require.Equal(t, "explicit", params["application_name"])
}

// A logged DSN is a leaked credential, and pgx leaves the raw one on an
// exported field even though its Error method redacts it.
func TestTheDSNNeverReachesAnError(t *testing.T) {
	t.Parallel()

	for name, config := range map[string]postgres.Config{
		"a malformed url": func() postgres.Config {
			c := baseConfig(t)
			c.URL = "postgres://someone:hunter2@db.internal:not-a-port/ledger"
			return c
		}(),
		"a malformed keyword string": func() postgres.Config {
			c := baseConfig(t)
			c.Password = "hunter2"
			c.SSLMode = "prefer"
			c.Port = -1
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.PoolConfig()
			require.Error(t, err)

			require.NotContains(t, err.Error(), "hunter2")
			require.NotContains(
				t,
				fmt.Sprintf("%+v", err),
				"hunter2",
				"the raw DSN survives on ParseConfigError.ConnString unless it is cleared",
			)

			var parseErr *pgconn.ParseConfigError
			if errors.As(err, &parseErr) {
				require.Equal(t, "[redacted]", parseErr.ConnString)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*postgres.Config){
		"an unknown sslmode":       func(c *postgres.Config) { c.SSLMode = "maybe" },
		"no host and no url":       func(c *postgres.Config) { c.Host = "" },
		"a negative pool size":     func(c *postgres.Config) { c.MaxConns = -1 },
		"more minimum than maxium": func(c *postgres.Config) { c.MinConns, c.MaxConns = 9, 4 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			config := baseConfig(t)
			mutate(&config)
			require.Error(t, config.Validate())
		})
	}
}

// pgxpool panics on a config it did not build, and a panic out of a shared
// constructor is not acceptable.
func TestOpenWithGuardsItsInput(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := baseConfig(t)

	_, err := postgres.OpenWith(ctx, testLogger(t), nil, config)
	require.Error(t, err)

	_, err = postgres.OpenWith(ctx, testLogger(t), &pgxpool.Config{}, config)
	require.Error(
		t,
		err,
		"a config that did not come from ParseConfig must report, not panic",
	)

	poolConfig, err := config.PoolConfig()
	require.NoError(t, err)
	_, err = postgres.OpenWith(ctx, nil, poolConfig, config)
	require.Error(t, err)
}

// The ping is the whole point: pgxpool.New would return a healthy-looking pool
// for an address nothing is listening on.
func TestRequireOnStart(t *testing.T) {
	t.Parallel()

	deadConfig := func(t *testing.T) postgres.Config {
		config := baseConfig(t)
		config.Host = "127.0.0.1"
		config.Port = 1 // nothing listens here
		config.SSLMode = "disable"
		config.ConnectTimeout = 500 * time.Millisecond
		config.HealthTimeout = 2 * time.Second
		return config
	}

	t.Run("on, the failure is at startup", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		config := deadConfig(t)
		config.RequireOnStart = true

		_, err := postgres.Open(ctx, testLogger(t), config)
		require.Error(t, err)
	})

	t.Run("off, a usable pool that still tells the truth", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		config := deadConfig(t)
		config.RequireOnStart = false

		db, err := postgres.Open(ctx, testLogger(t), config)
		require.NoError(t, err)
		defer func() { require.NoError(t, db.Close()) }()

		require.Error(t, db.Health(ctx), "Health must not pretend")

		// pgxpool.Pool.Close is guarded by a sync.Once, so this is safe.
		require.NoError(t, db.Close())
	})
}
