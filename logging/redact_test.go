package logging_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/config"
	"gitlab.internal/tuan.quang.tran/aioz-common/logging"
)

func TestRedactAttrRedactsTheDefaultKeys(t *testing.T) {
	t.Parallel()
	redact := logging.RedactAttr()

	keys := append(
		logging.RedactKeys(),
		"Password",
		"X-Api-Key",
		"access-token",
		"Set-Cookie",
		"AUTHORIZATION",
		"Client_Secret",
	)
	for _, key := range keys {
		got := redact(nil, slog.String(key, "hunter2"))
		require.Equal(
			t,
			key,
			got.Key,
			"the key stays, so the line still says what was there",
		)
		require.Equal(t, logging.Redacted, got.Value.String(), key)
	}
}

// Matching is exact, after lowercasing and turning - into _. A substring
// match would redact "passwordless" and "token_count" and teach people to
// ignore [redacted]; code, key and session are too common to own.
func TestRedactAttrLeavesOtherKeysAlone(t *testing.T) {
	t.Parallel()
	redact := logging.RedactAttr()

	for _, key := range []string{"code", "key", "session", "user", "email", "passwordless", "token_count", "tokens", "public_key"} {
		a := slog.String(key, "value")
		require.Equal(t, a, redact(nil, a), key)
	}
}

// slog.Group("password", "old", ..., "new", ...) hides its values under
// ordinary keys; a sensitive group name redacts everything in it.
func TestRedactAttrRedactsAWholeSensitiveGroup(t *testing.T) {
	t.Parallel()
	redact := logging.RedactAttr()

	require.Equal(
		t,
		logging.Redacted,
		redact(
			[]string{"request", "password"},
			slog.String("new", "x"),
		).Value.String(),
	)
	a := slog.String("id", "42")
	require.Equal(t, a, redact([]string{"request"}, a))
}

func TestRedactAttrTakesExtraKeys(t *testing.T) {
	t.Parallel()
	redact := logging.RedactAttr("wallet_seed")

	require.Equal(
		t,
		logging.Redacted,
		redact(nil, slog.String("Wallet-Seed", "x")).Value.String(),
	)
	require.Equal(
		t,
		logging.Redacted,
		redact(nil, slog.String("password", "x")).Value.String(),
		"the defaults stay",
	)
}

// The function runs for every attribute of every record: when nothing
// matches, which is nearly always, it must not allocate. Not parallel,
// because AllocsPerRun counts the whole process.
func TestRedactAttrAllocatesNothingWhenNothingMatches(t *testing.T) {
	redact := logging.RedactAttr("wallet_seed")
	groups := []string{"request"}
	a := slog.String("user_id", "42")
	long := slog.String(
		"a_key_far_longer_than_any_sensitive_key_could_ever_be_at_all",
		"x",
	)

	require.Zero(t, testing.AllocsPerRun(1000, func() {
		_ = redact(groups, a)
		_ = redact(nil, long)
	}))
}

func TestRedactKeysReturnsACopy(t *testing.T) {
	t.Parallel()

	keys := logging.RedactKeys()
	keys[0] = "changed"
	require.NotEqual(t, "changed", logging.RedactKeys()[0])
}

// The logger used before the config is read redacts too: a config error that
// logs the offending value is exactly when a secret gets printed. Not
// parallel, because it swaps os.Stderr.
func TestBootstrapRedacts(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	stderr := os.Stderr
	os.Stderr = w
	log := logging.Bootstrap()
	os.Stderr = stderr

	log.Info("reading config", "password", "hunter2", "user", "alice")
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)

	require.NotContains(t, string(out), "hunter2")
	require.Contains(t, string(out), logging.Redacted)
	require.Contains(t, string(out), "alice")
}

// End to end through the configured logger, read back from its file sink.
func TestTheServiceLoggerRedacts(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "service.log")
	cfg := defaults()
	cfg.Redact = []string{"wallet_seed"}
	cfg.File = logging.FileConfig{
		Enabled:       true,
		Path:          path,
		Format:        "json",
		BufferSize:    1024,
		FlushInterval: time.Hour,
		MaxSize:       1 << 20,
	}
	handler, _, err := logging.New(cfg)
	require.NoError(t, err)

	slog.New(handler).With("authorization", "Bearer abc").
		Info("login", "password", "hunter2", "wallet_seed", "seed words", "user_id", "42")
	// With under nested groups, the last of them sensitive.
	slog.New(handler).WithGroup("auth").WithGroup("password").
		With("current", "nested secret").Info("change")
	require.NoError(t, shutdown(handler))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	out := string(data)
	for _, secret := range []string{"hunter2", "seed words", "Bearer abc", "nested secret"} {
		require.NotContains(t, out, secret)
	}
	require.Contains(t, out, logging.Redacted)
	require.Contains(t, out, `"user_id":"42"`)
}

func TestRedactBindsEmpty(t *testing.T) {
	t.Parallel()

	var cfg struct {
		Log logging.Config
	}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NotPanics(t, func() { config.BindFlags(flags, &cfg) })
	require.Empty(t, cfg.Log.Redact)
}
