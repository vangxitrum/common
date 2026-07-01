package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
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
