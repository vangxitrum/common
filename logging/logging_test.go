package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/logger"
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

func TestWithOutputRedirectsTheConsole(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, _, err := logging.New(logging.Config{Level: "info", Format: "text"}, logging.WithOutput(&buf))
	require.NoError(t, err)

	slog.New(h).Info("redirected")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Contains(t, buf.String(), "redirected")
}

// A URL with no scheme parses and binds, and then every push fails at the
// transport with nothing in the configuration to point at.
func TestValidateRejectsAnElasticsearchURLWithoutAScheme(t *testing.T) {
	t.Parallel()

	cfg := defaults()
	cfg.Elasticsearch.Enabled = true
	cfg.Elasticsearch.URL = "es.example:9200"

	require.ErrorContains(t, cfg.Validate(), "http")
}

func TestValidateAcceptsBothSchemes(t *testing.T) {
	t.Parallel()

	for _, url := range []string{"http://es.example:9200", "https://es.example"} {
		cfg := defaults()
		cfg.Elasticsearch.Enabled = true
		cfg.Elasticsearch.URL = url

		require.NoError(t, cfg.Validate(), url)
	}
}

// custom-level holds named components to their own minimum level.
func TestCustomLevelQuietensAComponent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := defaults()
	cfg.Level = "debug"
	cfg.CustomLevel = "audit=warn, relay=error"
	h, _, err := logging.New(cfg, logging.WithOutput(&buf))
	require.NoError(t, err)

	log := slog.New(h)
	log.With("component", "audit").Info("audit chatter")
	log.With("component", "relay").Warn("relay warning")
	log.With("component", "worker").Debug("worker detail")
	require.NoError(t, h.Shutdown(context.Background()))

	require.NotContains(t, buf.String(), "audit chatter")
	require.NotContains(t, buf.String(), "relay warning")
	require.Contains(t, buf.String(), "worker detail")
}

func TestValidateRejectsABadCustomLevel(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"audit", "audit=loud", "=warn"} {
		cfg := defaults()
		cfg.CustomLevel = bad
		require.Error(t, cfg.Validate(), bad)
	}
}

// output selects stderr or a file as well as stdout.
func TestOutputWritesToAFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "out.log")
	cfg := defaults()
	cfg.Output = path
	h, _, err := logging.New(cfg)
	require.NoError(t, err)

	slog.New(h).Info("to a file")
	require.NoError(t, h.Shutdown(context.Background()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "to a file")
}

func TestParseStacktraceLevel(t *testing.T) {
	t.Parallel()

	off, err := logging.ParseStacktraceLevel("")
	require.NoError(t, err)
	require.Nil(t, off, "blank disables the automatic call stack")

	off, err = logging.ParseStacktraceLevel("   ")
	require.NoError(t, err)
	require.Nil(t, off)

	level, err := logging.ParseStacktraceLevel("error")
	require.NoError(t, err)
	require.Equal(t, slog.LevelError, level.Level())

	level, err = logging.ParseStacktraceLevel("critical")
	require.NoError(t, err)
	require.Equal(t, logger.LevelCritical, level.Level())

	_, err = logging.ParseStacktraceLevel("loud")
	require.Error(t, err, "an unknown level is a configuration error")
}

func TestValidateRejectsABadStacktraceLevel(t *testing.T) {
	t.Parallel()

	cfg := logging.Config{Level: "info", Format: "text", StacktraceLevel: "loud"}
	require.Error(t, cfg.Validate())

	cfg.StacktraceLevel = "error"
	require.NoError(t, cfg.Validate())

	cfg.StacktraceLevel = ""
	require.NoError(t, cfg.Validate(), "blank is valid: the feature is off")
}

func TestNewAttachesTheCallStackAtTheConfiguredLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler, _, err := logging.New(logging.Config{
		Level:           "debug",
		Format:          "json",
		StacktraceLevel: "warn",
		AsyncBufferSize: 64,
	}, logging.WithOutput(&buf))
	require.NoError(t, err)

	log := slog.New(handler)
	log.Info("quiet")
	log.Warn("loud")
	require.NoError(t, shutdown(handler))

	out := buf.String()
	require.NotEmpty(t, out)
	require.Contains(t, out, "callstack",
		"a record at the configured level carries one")
	require.Equal(t, 1, strings.Count(out, "callstack"),
		"and a record below it does not")
}

func TestNewWithNoStacktraceLevelAttachesNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler, _, err := logging.New(logging.Config{
		Level:           "debug",
		Format:          "json",
		AsyncBufferSize: 64,
	}, logging.WithOutput(&buf))
	require.NoError(t, err)

	slog.New(handler).Error("it broke")
	require.NoError(t, shutdown(handler))

	require.NotContains(t, buf.String(), "callstack")
}

func TestValidateRejectsAnUnknownElasticsearchEnv(t *testing.T) {
	t.Parallel()

	for _, env := range []string{"prod", "tue-staging", "Production", " staging"} {
		cfg := defaults()
		cfg.Elasticsearch.Enabled = true
		cfg.Elasticsearch.URL = "https://es.example"
		cfg.Elasticsearch.Env = env

		require.ErrorContains(t, cfg.Validate(), "log.elasticsearch.env", env)
	}
}

func TestValidateAcceptsTheKnownElasticsearchEnvs(t *testing.T) {
	t.Parallel()

	for _, env := range append(logging.Environments(), "") {
		cfg := defaults()
		cfg.Elasticsearch.Enabled = true
		cfg.Elasticsearch.URL = "https://es.example"
		cfg.Elasticsearch.Env = env

		require.NoError(t, cfg.Validate(), env)
	}
}

// TestNewStampsTheEnvOnShippedDocuments drives the whole path a service uses:
// the config section, logging.New, a log call, and the bulk request a real
// Elasticsearch would receive.
func TestNewStampsTheEnvOnShippedDocuments(t *testing.T) {
	t.Parallel()

	docs := make(chan map[string]any, 4)
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		for i := 1; i < len(lines); i += 2 {
			var doc map[string]any
			if json.Unmarshal([]byte(lines[i]), &doc) == nil {
				docs <- doc
			}
		}
	}))
	t.Cleanup(es.Close)

	cfg := defaults()
	cfg.Elasticsearch = logging.ElasticsearchConfig{
		Enabled:       true,
		URL:           es.URL,
		DataStream:    "logs-aioz-test",
		BatchSize:     1,
		FlushInterval: time.Hour,
		Timeout:       5 * time.Second,
		Service:       "svc",
		Env:           "production",
	}
	require.NoError(t, cfg.Validate())

	handler, _, err := logging.New(cfg, logging.WithOutput(io.Discard))
	require.NoError(t, err)
	slog.New(handler).Info("hello")
	require.NoError(t, shutdown(handler))

	select {
	case doc := <-docs:
		service := doc["service"].(map[string]any)
		require.Equal(t, "production", service["environment"])
		require.Equal(t, "svc", service["name"])
	case <-time.After(5 * time.Second):
		t.Fatal("no document reached Elasticsearch")
	}
}
