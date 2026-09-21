package logger

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWithAttrsThroughAsync verifies that attrs added via slog.Logger.With
// (-> Handler.WithAttrs -> asyncHandler.WithAttrs) actually reach the output.
func TestWithAttrsThroughAsync(t *testing.T) {
	var buf bytes.Buffer
	console := &consoleHandler{
		level:  slog.LevelInfo,
		format: FormatText,
		out:    &buf,
		mu:     new(sync.Mutex),
	}
	async := newAsyncHandler(console, 16)
	h := &Handler{inner: async, async: async}

	log := slog.New(h)
	log.With("trace_id", "abc123").Info("hello")

	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "trace_id") || !strings.Contains(out, "abc123") {
		t.Fatalf("With attrs missing from output: %q", out)
	}
}

// Output sends the console somewhere other than stdout.
func TestOutputReplacesStdout(t *testing.T) {
	var buf bytes.Buffer

	h, err := New(Config{Output: &buf})
	require.NoError(t, err)

	slog.New(h).Info("hello", "k", "v")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Contains(t, buf.String(), "hello")
	require.Contains(t, buf.String(), "k=v")
}

// Output discarded silences the console and nothing else: the sinks are what
// such a process wants the handler for.
func TestOutputDiscardStillReachesSinks(t *testing.T) {
	sink := &recordingSink{}

	h, err := New(Config{Output: io.Discard})
	require.NoError(t, err)
	h.sinks = append(h.sinks, sink)
	h.inner.(*asyncHandler).inner.(*consoleHandler).sinks = h.sinks

	slog.New(h).Info("quiet console", "k", "v")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Len(t, sink.lines, 1)
	require.Equal(t, "v", sink.lines[0]["k"])
}

// The text console names a source by its package directory and file, not by
// the absolute path of whatever checkout built the binary.
func TestTextConsoleShortensTheSource(t *testing.T) {
	var buf bytes.Buffer
	h, err := New(Config{Output: &buf, AddSource: true})
	require.NoError(t, err)

	slog.New(h).Info("hello")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Contains(t, buf.String(), "src=logger/with_test.go:")
	require.NotContains(t, buf.String(), "src=/")
}

func TestShortSource(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/src/depin/cmd/coord/main.go:711": "coord/main.go:711",
		"main.go:3":   "main.go:3",
		"/main.go:3":  "/main.go:3",
		"a/main.go:3": "a/main.go:3",
	} {
		require.Equal(t, want, shortSource(in), in)
	}
}
