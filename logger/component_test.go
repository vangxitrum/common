package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// newComponentTestLogger returns a logger writing text into buf at base, with
// the given per-component levels.
func newComponentTestLogger(t *testing.T, buf *bytes.Buffer, base slog.Level, levels map[string]slog.Level) (*slog.Logger, *Handler) {
	t.Helper()
	h, err := New(Config{Level: base, Output: buf, ComponentLevels: levels})
	require.NoError(t, err)
	return slog.New(h), h
}

// A component named in ComponentLevels is held to its own level, whichever
// way the process's level has been set.
func TestComponentLevelRaisesTheThreshold(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelDebug, map[string]slog.Level{"audit": slog.LevelWarn})

	audit := log.With("component", "audit")
	audit.Info("audit chatter")
	audit.Warn("audit problem")
	log.With("component", "relay").Debug("relay detail")
	require.NoError(t, h.Shutdown(context.Background()))

	out := buf.String()
	require.NotContains(t, out, "audit chatter")
	require.Contains(t, out, "audit problem")
	require.Contains(t, out, "relay detail")
}

// The component can arrive with the record rather than through With.
func TestComponentLevelAppliesToARecordAttribute(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelDebug, map[string]slog.Level{"audit": slog.LevelError})

	log.Warn("inline", "component", "audit")
	log.Warn("other", "component", "relay")
	require.NoError(t, h.Shutdown(context.Background()))

	require.NotContains(t, buf.String(), "inline")
	require.Contains(t, buf.String(), "other")
}

// The innermost component is the one a line belongs to, as with a nested
// named logger, so a sub-logger can be quieter than its parent.
func TestComponentLevelUsesTheInnermostComponent(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelDebug, map[string]slog.Level{"worker": slog.LevelError})

	log.With("component", "audit").With("component", "worker").Warn("quiet")
	log.With("component", "worker").With("component", "audit").Warn("loud")
	require.NoError(t, h.Shutdown(context.Background()))

	require.NotContains(t, buf.String(), "quiet")
	require.Contains(t, buf.String(), "loud")
}

// Without any per-component levels nothing changes, and the component is still
// written as an ordinary attribute.
func TestNoComponentLevelsLeavesOutputAlone(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	log.With("component", "audit").Info("hello")
	require.NoError(t, h.Shutdown(context.Background()))

	require.True(t, strings.Contains(buf.String(), "component=audit"), buf.String())
}
