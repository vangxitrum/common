package rabbitmq

import (
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/config"
)

// testConfig is what config.Bind produces from the struct tags, so a test
// starts from the defaults an operator actually gets.
func testConfig() Config {
	return Config{
		Name:              "aioz",
		Address:           "127.0.0.1:5672",
		Vhost:             "/",
		Username:          "guest",
		DialTimeout:       10 * time.Second,
		Heartbeat:         10 * time.Second,
		ReconnectDelay:    time.Second,
		ReconnectMaxDelay: 30 * time.Second,
	}
}

func TestDialURL(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"defaults": {
			mutate: func(*Config) {},
			want:   "amqp://guest:@127.0.0.1:5672/",
		},
		"credentials": {
			mutate: func(c *Config) { c.Username = "svc"; c.Password = "p@ss word" },
			want:   "amqp://svc:p%40ss%20word@127.0.0.1:5672/",
		},
		"named vhost": {
			mutate: func(c *Config) { c.Vhost = "events" },
			want:   "amqp://guest:@127.0.0.1:5672/events",
		},
		"tls": {
			mutate: func(c *Config) { c.TLS = true },
			want:   "amqps://guest:@127.0.0.1:5672/",
		},
		"explicit url wins": {
			mutate: func(c *Config) { c.URL = "amqp://other:9/vhost"; c.Address = "ignored:5672" },
			want:   "amqp://other:9/vhost",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig()
			tc.mutate(&cfg)
			require.Equal(t, tc.want, cfg.DialURL())
		})
	}
}

// ChannelMax is an int in the config because amqp's uint16 is not a bindable
// type, so the range it really has has to be checked somewhere.
func TestAMQPRejectsAnOutOfRangeChannelMax(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]int{
		"negative": -1,
		"too big":  65536,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig()
			cfg.ChannelMax = value

			_, err := cfg.amqp()
			require.Error(t, err)
			require.Contains(t, err.Error(), "channel-max")
		})
	}
}

func TestAMQPConfig(t *testing.T) {
	t.Parallel()

	t.Run("discrete fields", func(t *testing.T) {
		t.Parallel()

		cfg := testConfig()
		cfg.Vhost = "events"
		cfg.ChannelMax = 128
		cfg.FrameSize = 4096

		out, err := cfg.amqp()
		require.NoError(t, err)
		require.Equal(t, "events", out.Vhost)
		require.Equal(t, uint16(128), out.ChannelMax)
		require.Equal(t, 4096, out.FrameSize)
		require.Equal(t, 10*time.Second, out.Heartbeat)
		require.NotNil(t, out.Dial)
		require.Nil(t, out.TLSClientConfig)
	})

	// DialConfig takes the vhost from the config and not the URL, so a
	// URL-only configuration would otherwise silently land on "/".
	t.Run("vhost comes out of the url", func(t *testing.T) {
		t.Parallel()

		cfg := testConfig()
		cfg.URL = "amqp://svc:secret@broker:5672/events"

		out, err := cfg.amqp()
		require.NoError(t, err)
		require.Equal(t, "events", out.Vhost)
	})

	t.Run("amqps url implies tls", func(t *testing.T) {
		t.Parallel()

		cfg := testConfig()
		cfg.URL = "amqps://broker:5671/"

		out, err := cfg.amqp()
		require.NoError(t, err)
		require.NotNil(t, out.TLSClientConfig)
		require.False(t, out.TLSClientConfig.InsecureSkipVerify)
	})

	t.Run("skip verify is opt-in", func(t *testing.T) {
		t.Parallel()

		cfg := testConfig()
		cfg.TLS = true
		cfg.TLSSkipVerify = true

		out, err := cfg.amqp()
		require.NoError(t, err)
		require.NotNil(t, out.TLSClientConfig)
		require.True(t, out.TLSClientConfig.InsecureSkipVerify)
	})
}

// A malformed URL must not put the password in the error string.
func TestAMalformedURLDoesNotLeakThePassword(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.URL = "amqp://svc:s3cret@broker:5672/\x7f"

	_, err := cfg.amqp()
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret")
}

// The tags have to survive Bind, which panics on a secret carrying a shipped
// value. A local broker really is guest/guest, so Password uses the sanctioned
// devDefault/releaseDefault pair, and this proves the release build comes up
// empty.
func TestConfigBinds(t *testing.T) {
	t.Parallel()

	var cfg struct {
		RabbitMQ Config
	}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NotPanics(t, func() { config.BindFlags(flags, &cfg) })

	require.Equal(t, "aioz", cfg.RabbitMQ.Name)
	require.Equal(t, "127.0.0.1:5672", cfg.RabbitMQ.Address)
	require.Equal(t, "guest", cfg.RabbitMQ.Username)
	require.Equal(t, 10*time.Second, cfg.RabbitMQ.DialTimeout)
	require.False(t, cfg.RabbitMQ.RequireOnStart)

	require.Empty(t, cfg.RabbitMQ.Password)
	require.Empty(t, cfg.RabbitMQ.URL)
}

func TestConfigBindsWithDevDefaults(t *testing.T) {
	t.Parallel()

	var cfg struct {
		RabbitMQ Config
	}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NotPanics(t, func() { config.BindFlags(flags, &cfg, config.UseDevDefaults()) })

	require.Equal(t, "guest", cfg.RabbitMQ.Password)
}
