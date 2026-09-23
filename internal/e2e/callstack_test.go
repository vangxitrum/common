package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/depin"
	"gitlab.internal/tuan.quang.tran/aioz-common/logger"
	"gitlab.internal/tuan.quang.tran/aioz-common/logging"
)

// A service configures the call stack through logging.Config, so that is what
// these drive: the flag value in, a real failure logged, and the line an
// operator would read out.

func TestServiceConfiguredCallStackNamesTheHandlerThatLogged(t *testing.T) {
	var buf bytes.Buffer
	handler, _, err := logging.New(logging.Config{
		Level:           "debug",
		Format:          "text",
		AddSource:       true,
		StacktraceLevel: "error",
		AsyncBufferSize: 64,
	}, logging.WithOutput(&buf))
	require.NoError(t, err)

	fetchAndLog(slog.New(handler))
	require.NoError(t, handler.Shutdown(context.Background()))

	out := buf.String()
	require.Contains(t, out, "could not fetch the file")
	require.Contains(t, out, "callstack:",
		"printed as a labeled block, not escaped onto the line")
	require.Contains(t, out, "internal/e2e.fetchAndLog",
		"the innermost frame is the function that logged")
	require.Contains(
		t,
		out,
		"internal/e2e.TestServiceConfiguredCallStackNamesTheHandlerThatLogged",
		"and its caller is above it",
	)
	require.NotContains(t, out, "log/slog.",
		"slog's dispatch is not part of the answer")
	require.NotContains(t, out, "\\n", "nothing escaped onto one line")
}

func TestCallStackIsOffByDefault(t *testing.T) {
	var buf bytes.Buffer
	handler, _, err := logging.New(logging.Config{
		Level:           "debug",
		Format:          "text",
		AsyncBufferSize: 64,
	}, logging.WithOutput(&buf))
	require.NoError(t, err)

	fetchAndLog(slog.New(handler))
	require.NoError(t, handler.Shutdown(context.Background()))

	require.NotContains(t, buf.String(), "callstack",
		"a service that did not ask for it pays nothing and sees nothing")
}

func TestCallStackShipsBesideTheCallSiteInJSON(t *testing.T) {
	// JSON is what the file sink, Graylog and Elasticsearch serialize, so this
	// is the shape a dashboard receives.
	var buf bytes.Buffer
	handler, _, err := logging.New(logging.Config{
		Level:           "debug",
		Format:          "json",
		AddSource:       true,
		StacktraceLevel: "error",
		AsyncBufferSize: 64,
	}, logging.WithOutput(&buf))
	require.NoError(t, err)

	fetchAndLog(slog.New(handler))
	require.NoError(t, handler.Shutdown(context.Background()))

	var line map[string]any
	require.NoError(
		t,
		json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &line),
	)

	require.Contains(t, line, "callstack")
	require.Contains(t, line["callstack"], "internal/e2e.fetchAndLog")
	require.Contains(t, line, "source",
		"the call site is still there; callstack is the rest of it")
}

func TestCriticalCarriesItsCallStackWithoutBeingConfigured(t *testing.T) {
	var buf bytes.Buffer
	handler, _, err := logging.New(logging.Config{
		Level:           "debug",
		Format:          "json",
		AsyncBufferSize: 64,
	}, logging.WithOutput(&buf))
	require.NoError(t, err)

	logger.Critical(slog.New(handler), "cannot continue")
	require.NoError(t, handler.Shutdown(context.Background()))

	var line map[string]any
	require.NoError(
		t,
		json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &line),
	)

	require.Contains(t, line, "callstack",
		"Critical attaches one whatever StacktraceLevel says")
	require.Contains(t, line["callstack"],
		"TestCriticalCarriesItsCallStackWithoutBeingConfigured")
	require.NotContains(t, line, "stacktrace",
		"and it is not filed as an error's stack, since there is no error")
}

// fetchAndLog is a stand-in for the service layer that handles a failure.
func fetchAndLog(log *slog.Logger) {
	storage := depin.NewLog(slog.New(slog.DiscardHandler))
	err := storage.Download(context.Background(), [16]byte{}, discardWriter{})
	log.Error("could not fetch the file", "err", err)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
