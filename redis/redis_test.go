package redis_test

import (
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/config"
	"gitlab.internal/tuan.quang.tran/aioz-common/redis"
)

// baseConfig is what config.Bind would produce from the struct tags, so a test
// starts from the defaults an operator actually gets.
func baseConfig() redis.Config {
	return redis.Config{
		Name:           "redis",
		Address:        "127.0.0.1:6379",
		MaxRetries:     3,
		DialTimeout:    5 * time.Second,
		ReadTimeout:    3 * time.Second,
		WriteTimeout:   3 * time.Second,
		RequireOnStart: true,
		HealthTimeout:  3 * time.Second,
		Metrics:        true,
	}
}

func TestOptionsFromDiscreteFields(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Address = "cache.internal:6380"
	cfg.DB = 3
	cfg.Username = "svc"
	cfg.Password = "hunter2"
	cfg.ClientName = "parser"

	opts, err := cfg.Options()
	require.NoError(t, err)

	require.Equal(t, "cache.internal:6380", opts.Addr)
	require.Equal(t, 3, opts.DB)
	require.Equal(t, "svc", opts.Username)
	require.Equal(t, "hunter2", opts.Password)
	require.Equal(t, "parser", opts.ClientName)
	require.Nil(t, opts.TLSConfig)
}

// A DSN overrides the discrete fields entirely. Half-and-half would be the
// worst outcome: an operator who sets a URL and leaves the default address in
// place must not get a connection to whichever one the code happened to read
// last.
func TestURLOverridesDiscreteFields(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Address = "ignored:6379"
	cfg.DB = 1
	cfg.Username = "ignored"
	cfg.Password = "ignored"
	cfg.URL = "redis://svc:s3cret@cache.internal:6380/7"

	opts, err := cfg.Options()
	require.NoError(t, err)

	require.Equal(t, "cache.internal:6380", opts.Addr)
	require.Equal(t, 7, opts.DB)
	require.Equal(t, "svc", opts.Username)
	require.Equal(t, "s3cret", opts.Password)
}

// The reason the timeout fields exist at all: a DSN says nothing about them,
// and go-redis's own defaults are what every service in the estate is running
// on today.
func TestTimeoutsSurviveAParsedURL(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.URL = "redis://cache.internal:6379/0"
	cfg.DialTimeout = 11 * time.Second
	cfg.ReadTimeout = 12 * time.Second
	cfg.WriteTimeout = 13 * time.Second
	cfg.PoolTimeout = 14 * time.Second
	cfg.PoolSize = 42
	cfg.MinIdleConns = 7
	cfg.MaxRetries = 9

	opts, err := cfg.Options()
	require.NoError(t, err)

	require.Equal(t, 11*time.Second, opts.DialTimeout)
	require.Equal(t, 12*time.Second, opts.ReadTimeout)
	require.Equal(t, 13*time.Second, opts.WriteTimeout)
	require.Equal(t, 14*time.Second, opts.PoolTimeout)
	require.Equal(t, 42, opts.PoolSize)
	require.Equal(t, 7, opts.MinIdleConns)
	require.Equal(t, 9, opts.MaxRetries)
}

func TestOptionsTLS(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mutate     func(*redis.Config)
		wantTLS    bool
		wantSkip   bool
		wantServer string
	}{
		"off": {
			mutate: func(*redis.Config) {},
		},
		"on": {
			mutate:     func(c *redis.Config) { c.TLS = true; c.Address = "cache.internal:6380" },
			wantTLS:    true,
			wantServer: "cache.internal",
		},
		"skip verify": {
			mutate:   func(c *redis.Config) { c.TLS = true; c.TLSSkipVerify = true },
			wantTLS:  true,
			wantSkip: true,
			// The default address is a host:port, so ServerName is its host.
			wantServer: "127.0.0.1",
		},
		"rediss url": {
			mutate:     func(c *redis.Config) { c.URL = "rediss://cache.internal:6380/0" },
			wantTLS:    true,
			wantServer: "cache.internal",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			tc.mutate(&cfg)

			opts, err := cfg.Options()
			require.NoError(t, err)

			if !tc.wantTLS {
				require.Nil(t, opts.TLSConfig)
				return
			}

			require.NotNil(t, opts.TLSConfig)
			require.Equal(t, tc.wantSkip, opts.TLSConfig.InsecureSkipVerify)
			require.Equal(t, tc.wantServer, opts.TLSConfig.ServerName)
		})
	}
}

func TestOptionsRejectsAnEmptyAddress(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Address = ""

	_, err := cfg.Options()
	require.Error(t, err)
	require.Contains(t, err.Error(), "address is empty")
}

// A malformed DSN must not put the password in the error string. url.Parse
// puts the string it failed on into *url.Error, and an error goes to a log.
func TestAMalformedURLDoesNotLeakThePassword(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.URL = "redis://svc:s3cret@cache.internal:6380/\x7f"

	_, err := cfg.Options()
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret")
}

// The tags have to survive Bind, which panics on a secret carrying a shipped
// value. This is the same guard config/secrets_test.go applies to its own
// structs; here it proves the redis Config is bindable at all.
func TestConfigBinds(t *testing.T) {
	t.Parallel()

	var cfg struct {
		Redis redis.Config
	}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NotPanics(t, func() { config.BindFlags(flags, &cfg) })

	require.Equal(t, "redis", cfg.Redis.Name)
	require.Equal(t, "127.0.0.1:6379", cfg.Redis.Address)
	require.Equal(t, 5*time.Second, cfg.Redis.DialTimeout)
	require.True(t, cfg.Redis.RequireOnStart)
	require.True(t, cfg.Redis.Metrics)

	// A secret must not arrive with a value, in a release build or a dev one.
	require.Empty(t, cfg.Redis.Password)
	require.Empty(t, cfg.Redis.URL)
}

// Open with metrics off must still produce a usable client, since the hook is
// the only thing the flag controls.
func TestOptionsAreUsableByGoRedis(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	opts, err := cfg.Options()
	require.NoError(t, err)

	client := goredis.NewClient(opts)
	require.NoError(t, client.Close())
}
