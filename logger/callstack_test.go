package logger

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs/v2"
)

// jsonLogger returns a logger writing one JSON object per line into buf, which
// is what every sink is handed too.
func jsonLogger(
	t *testing.T,
	stackLevel slog.Leveler,
) (*slog.Logger, *strings.Builder) {
	t.Helper()
	var buf strings.Builder
	handler, err := New(Config{
		Level:           slog.LevelDebug,
		Format:          FormatJSON,
		StacktraceLevel: stackLevel,
		Output:          &buf,
	})
	require.NoError(t, err)
	return slog.New(handler), &buf
}

// lines parses what was written. Shutdown must have run first.
func lines(t *testing.T, buf *strings.Builder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &line), "raw: %s", raw)
		out = append(out, line)
	}
	return out
}

func callstackOf(t *testing.T, line map[string]any) string {
	t.Helper()
	v, ok := line[callstackKey]
	require.True(t, ok, "no %s in %v", callstackKey, line)
	s, ok := v.(string)
	require.True(t, ok, "%s is %T, want string", callstackKey, v)
	return s
}

var testTag = errs.Tag("callstacktest")

func taggedError() error { return testTag.Errorf("it broke down there") }

func TestStacktraceLevelAttachesTheCallStack(t *testing.T) {
	log, buf := jsonLogger(t, slog.LevelError)

	log.Error("it broke")

	require.NoError(t, closeAndFlush(t, log))
	got := lines(t, buf)
	require.Len(t, got, 1)

	frames := strings.Split(callstackOf(t, got[0]), "\n")
	require.NotEmpty(t, frames)
	require.Contains(t, frames[0], "TestStacktraceLevelAttachesTheCallStack",
		"the innermost frame is the code that logged, not slog or this handler")
	for _, frame := range frames {
		require.NotContains(t, frame, "log/slog.",
			"slog's own dispatch is plumbing")
		require.NotContains(t, frame, "logger.(*Handler).Handle",
			"and so is the handler that captured it")
		require.NotContains(t, frame, "runtime.Callers")
	}
}

func TestStacktraceLevelIgnoresLowerLevels(t *testing.T) {
	log, buf := jsonLogger(t, slog.LevelError)

	log.Info("all well")
	log.Warn("a bit off")

	require.NoError(t, closeAndFlush(t, log))
	for _, line := range lines(t, buf) {
		require.NotContains(t, line, callstackKey,
			"a record below the threshold carries no call stack")
	}
}

func TestStacktraceLevelIncludesHigherLevels(t *testing.T) {
	log, buf := jsonLogger(t, slog.LevelWarn)

	log.Warn("a bit off")
	log.Error("it broke")

	require.NoError(t, closeAndFlush(t, log))
	got := lines(t, buf)
	require.Len(t, got, 2)
	for _, line := range got {
		require.NotEmpty(t, callstackOf(t, line))
	}
}

func TestNoStacktraceLevelMeansNoCallStack(t *testing.T) {
	log, buf := jsonLogger(t, nil)

	log.Error("it broke")

	require.NoError(t, closeAndFlush(t, log))
	got := lines(t, buf)
	require.Len(t, got, 1)
	require.NotContains(t, got[0], callstackKey,
		"the call stack is off unless StacktraceLevel is set")
}

func TestARecordsOwnCallstackIsNotOverwritten(t *testing.T) {
	// Critical attaches its own, captured closer to the caller.
	log, buf := jsonLogger(t, slog.LevelError)

	log.Error("it broke", callstackKey, "mine")

	require.NoError(t, closeAndFlush(t, log))
	got := lines(t, buf)
	require.Len(t, got, 1)
	require.Equal(t, "mine", got[0][callstackKey])
}

func TestTheCallStackIsTheOnlyStackOnAnErrorRecord(t *testing.T) {
	// The error's own stack is not logged: callstack, how we got to the log
	// statement, is the one stack a record carries.
	log, buf := jsonLogger(t, slog.LevelError)

	log.Error("it broke", "err", taggedError())

	require.NoError(t, closeAndFlush(t, log))
	got := lines(t, buf)
	require.Len(t, got, 1)
	require.Equal(t, "callstacktest: it broke down there", got[0]["err"])
	require.NotContains(t, got[0], "stacktrace")
	require.NotEmpty(t, callstackOf(t, got[0]))
}

func TestTheCallStackIsPrintedAsABlockOnTheConsole(t *testing.T) {
	var buf strings.Builder
	handler, err := New(Config{
		Level:           slog.LevelDebug,
		Format:          FormatText,
		StacktraceLevel: slog.LevelError,
		Output:          &buf,
	})
	require.NoError(t, err)

	slog.New(handler).Error("it broke")
	require.NoError(t, handler.Shutdown(context.Background()))

	out := buf.String()
	require.Contains(t, out, "callstack:",
		"labeled, indented under the line - not escaped onto it")
	require.NotContains(t, out, callstackKey+"=",
		"and never as a key=value pair")
	require.NotContains(t, out, "\\n")
}

// closeAndFlush shuts the handler down so the async consumer has drained.
func closeAndFlush(t *testing.T, log *slog.Logger) error {
	t.Helper()
	h, ok := log.Handler().(*Handler)
	require.True(t, ok)
	return h.Shutdown(context.Background())
}

func TestTrimFrame(t *testing.T) {
	t.Parallel()

	for frame, want := range map[string]string{
		"10.0.0.50/tuan.quang.tran/vms-v2/internal/app/user.(*Service).DeleteExpiredRefreshTokens:254": "vms-v2/internal/app/user.(*Service).DeleteExpiredRefreshTokens:254",
		"gitlab.internal/tuan.quang.tran/aioz-common/cycle.(*Cycle).Run:84":                            "aioz-common/cycle.(*Cycle).Run:84",
		"main.(*Service).Delete:30":     "main.(*Service).Delete:30",
		"net/http.(*conn).serve:2092":   "net/http.(*conn).serve:2092",
		"example.com/pkg.Func:1":        "example.com/pkg.Func:1",
		"gopkg.in/yaml.v3.Unmarshal:10": "gopkg.in/yaml.v3.Unmarshal:10",
	} {
		require.Equal(t, want, trimFrame(frame), frame)
	}
}

func TestIsPlumbing(t *testing.T) {
	t.Parallel()

	for frame, want := range map[string]bool{
		"runtime.goexit:1700":                             true,
		"runtime/pprof.Do:51":                             true,
		"golang.org/x/sync/errgroup.(*Group).Go.func1:93": true,
		"gitlab.internal/tuan.quang.tran/aioz-common/lifecycle.(*Group).Run.func1:110": true,
		"gitlab.internal/tuan.quang.tran/aioz-common/cycle.(*Cycle).Run:84":            false,
		"10.0.0.50/tuan.quang.tran/vms-v2/internal/utils/chore.(*Chore).Run:76":        false,
	} {
		require.Equal(t, want, isPlumbing(frame), frame)
	}
}

func TestFormatBlock(t *testing.T) {
	t.Parallel()

	require.Empty(t, formatBlock("callstack", nil))
	require.Equal(
		t,
		"\n  callstack:\n    a:1\n    b:2",
		formatBlock("callstack", []string{"a:1", "b:2"}),
	)
}
