package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs/v2"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
)

var userError = errs.Tag("user")

const failure = `user: relation "refresh_tokens" does not exist`

func failing() error {
	return userError.Wrap(
		errors.New(`relation "refresh_tokens" does not exist`),
	)
}

// zeebo/errs's error has no exported fields, so without the conversion the
// JSON output and every sink got {} for it. Its stack is not logged: the
// call stack of the log statement is what StacktraceLevel and Critical add.
func TestAnErrorIsLoggedAsItsMessageAlone(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sink := &recordingSink{}
	slog.New(testConsole(&buf, nil, sink)).
		Error("chore failed", "err", failing())

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, failure, line["err"])
	require.NotContains(t, line, "stacktrace")

	require.Len(t, sink.lines, 1)
	require.Equal(t, failure, sink.lines[0]["err"])
	require.NotContains(t, sink.lines[0], "stacktrace")
}

// The text console prints the message on the line, never the escaped %+v
// that slog.TextHandler writes, and no block under it.
func TestTextPrintsAnErrorAsItsMessageAlone(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	console := testConsole(&buf, nil)
	console.format = FormatText
	slog.New(console).Error("chore failed", "err", failing())

	out := buf.String()
	require.Contains(t, out, "err="+failure)
	require.NotContains(t, out, `\n\t`)
	require.NotContains(t, out, "stack:")
	require.NotContains(t, out, "logger.failing")
	require.Equal(t, 1, strings.Count(out, "\n"), "one line, no block")
}

// apierr.Error is a LogValuer that resolves to its message.
func TestAnAPIErrorIsLoggedAsItsMessage(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(testConsole(&buf, nil)).
		Error("request failed", "err", apierr.Internal("db", failing()))

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.IsType(t, "", line["err"])
	require.NotContains(t, line, "stacktrace")
}

// Critical carries the call stack of the log statement and nothing taken
// from the error.
func TestCriticalCarriesOnlyItsCallStack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	Critical(slog.New(testConsole(&buf, nil)), "lost", "err", failing())

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, failure, line["err"])
	require.Contains(t, line[callstackKey], "TestCriticalCarriesOnlyItsCallStack")
	require.NotContains(t, line[callstackKey], "logger.failing")
	require.NotContains(t, line, "stacktrace")
}

// The aioz-stream symptom, reproduced on a plain slog.TextHandler, and gone
// with ErrorAttr.
func TestErrorAttrStopsTheTextHandlerDumpingTheStack(t *testing.T) {
	t.Parallel()

	var before, after bytes.Buffer
	slog.New(slog.NewTextHandler(&before, nil)).
		Error("failed", "err", failing())
	slog.New(slog.NewTextHandler(&after, &slog.HandlerOptions{ReplaceAttr: ErrorAttr})).
		Error("failed", "err", failing())

	require.Contains(t, before.String(), `\n\t`)
	require.NotContains(t, after.String(), `\n\t`)
	require.Contains(
		t,
		after.String(),
		`err="user: relation \"refresh_tokens\" does not exist"`,
	)
}

func TestErrorAttrLeavesOtherValuesAlone(t *testing.T) {
	t.Parallel()

	for _, a := range []slog.Attr{slog.String("k", "v"), slog.Int("n", 1), slog.Any("s", struct{ A int }{1})} {
		require.True(t, a.Equal(ErrorAttr(nil, a)), a.Key)
	}
}
