package logging_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/logging"
)

func defaults() logging.Config {
	return logging.Config{Level: "info", Format: "text", AsyncBufferSize: 64}
}

// TestLevelVarIsLive pins the contract the debug server's /logging endpoint
// depends on: the handler must observe later writes to the returned LevelVar,
// which only holds if logger.Config.Level is the LevelVar itself rather than
// its value at construction time.
func TestLevelVarIsLive(t *testing.T) {
	t.Parallel()

	handler, level, err := logging.New(defaults())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, shutdown(handler)) })

	ctx := context.Background()
	require.False(t, handler.Enabled(ctx, slog.LevelDebug))

	level.Set(slog.LevelDebug)
	require.True(t, handler.Enabled(ctx, slog.LevelDebug))
}

// TestParsingRoundTrips is the anti-drift check: every value offered by shell
// completion has to be one the service will actually take.
func TestParsingRoundTrips(t *testing.T) {
	t.Parallel()

	for _, level := range logging.Levels() {
		_, err := logging.ParseLevel(level)
		require.NoError(t, err, "suggested level %q is rejected", level)
	}
	for _, format := range logging.Formats() {
		_, err := logging.ParseFormat(format)
		require.NoError(t, err, "suggested format %q is rejected", format)
	}

	_, err := logging.ParseLevel("verbose")
	require.Error(t, err)
	_, err = logging.ParseFormat("logfmt")
	require.Error(t, err)
}

func TestValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, defaults().Validate())

	for name, breakIt := range map[string]func(*logging.Config){
		"bad level":           func(c *logging.Config) { c.Level = "verbose" },
		"bad format":          func(c *logging.Config) { c.Format = "logfmt" },
		"graylog without url": func(c *logging.Config) { c.Graylog.Enabled = true },
		"file without path": func(c *logging.Config) {
			c.File.Enabled = true
			c.File.Path = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaults()
			breakIt(&cfg)
			require.Error(t, cfg.Validate())
		})
	}
}

// TestBadValuesAreErrors checks that unusable settings come back as errors
// rather than panics or a half-built handler.
func TestBadValuesAreErrors(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*logging.Config){
		"level":       func(c *logging.Config) { c.Level = "verbose" },
		"format":      func(c *logging.Config) { c.Format = "logfmt" },
		"file format": func(c *logging.Config) { c.File.Enabled = true; c.File.Format = "logfmt" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := defaults()
			mutate(&cfg)

			handler, level, err := logging.New(cfg)
			require.Error(t, err)
			require.Nil(t, handler)
			require.Nil(t, level)
		})
	}
}

func shutdown(handler interface{ Shutdown(context.Context) error }) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return handler.Shutdown(ctx)
}
