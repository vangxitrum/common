package logger

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingSink struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (s *recordingSink) Handle(
	_ slog.Level,
	_ time.Time,
	_ string,
	attrs map[string]any,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := make(map[string]any, len(attrs))
	for k, v := range attrs {
		line[k] = v
	}
	s.lines = append(s.lines, line)
	return nil
}

func (s *recordingSink) Close() error { return nil }

func (s *recordingSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprint(s.lines)
}

func redactPassword(_ []string, a slog.Attr) slog.Attr {
	switch {
	case strings.HasPrefix(a.Key, "password"):
		return slog.String(a.Key, "[redacted]")
	case a.Key == "drop":
		return slog.Attr{}
	}
	return a
}

func testConsole(
	buf *bytes.Buffer,
	replace func([]string, slog.Attr) slog.Attr,
	sinks ...Sink,
) *consoleHandler {
	return &consoleHandler{
		level:   slog.LevelInfo,
		format:  FormatJSON,
		out:     buf,
		mu:      new(sync.Mutex),
		sinks:   sinks,
		replace: replace,
	}
}

// The context attributes are read inside the console handler, so a handler
// wrapped around this one could never redact them: ReplaceAttr has to run
// here, over every source, before anything reaches stdout or a sink.
func TestReplaceAttrCoversEverySource(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sink := &recordingSink{}
	h := testConsole(&buf, redactPassword, sink)

	ctx := context.WithValue(context.Background(), ctxKey{},
		[]slog.Attr{slog.String("password_ctx", "from-context")})
	slog.New(h).With("password_with", "from-with").InfoContext(ctx, "login",
		"password_record", "from-record",
		slog.Group("req", "password", "from-group"))
	slog.New(h).WithGroup("g").With("password_gwith", "from-group-with").
		Info("again", "password_grecord", "from-with-group")

	for _, out := range []string{buf.String(), sink.String()} {
		for _, secret := range []string{"from-context", "from-with", "from-record", "from-group", "from-group-with", "from-with-group"} {
			require.NotContains(t, out, secret)
		}
		require.Contains(t, out, "[redacted]")
	}
}

func TestReplaceAttrReceivesTheGroupPath(t *testing.T) {
	t.Parallel()

	var seen [][]string
	var buf bytes.Buffer
	h := testConsole(&buf, func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == "leaf" {
			seen = append(seen, slices.Clone(groups))
		}
		return a
	})

	slog.New(h).WithGroup("a").Info("m", slog.Group("b", "leaf", 1))
	slog.New(h).Info("m", "leaf", 2)

	require.Equal(t, [][]string{{"a", "b"}, nil}, seen)
}

func TestReplaceAttrCanDropAnAttr(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(testConsole(&buf, redactPassword)).
		Info("m", "drop", "gone", "kept", "here")

	require.NotContains(t, buf.String(), "drop")
	require.NotContains(t, buf.String(), "gone")
	require.Contains(t, buf.String(), `"kept":"here"`)
}

func TestWithoutReplaceAttrNothingChanges(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(testConsole(&buf, nil)).Info("m", "password", "visible")

	require.Contains(t, buf.String(), `"password":"visible"`)
}
