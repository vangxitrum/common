package depin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// capture returns a logger writing one JSON object per record into buf.
func capture(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(
		slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}),
	)
}

// lines decodes every record written so far.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal(raw, &line))
		out = append(out, line)
	}
	return out
}

// The SDK's fields have to arrive as attributes, whatever their zap type.
func TestBridgeCarriesFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	newSDKLogger(capture(&buf, slog.LevelDebug)).
		Info("coord.GetDownloadInfo: done",
			zap.String("file_id", "abc"),
			zap.Int("pieces", 42),
			zap.Bool("cached", true))

	got := lines(t, &buf)
	require.Len(t, got, 1)
	require.Equal(t, "coord.GetDownloadInfo: done", got[0]["msg"])
	require.Equal(t, "abc", got[0]["file_id"])
	require.EqualValues(t, 42, got[0]["pieces"])
	require.Equal(t, true, got[0]["cached"])
}

// A named sublogger is how the SDK labels its subsystems, e.g.
// logger.Named("p2pmonitor"), and that label is worth keeping.
func TestBridgeKeepsTheLoggerName(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	newSDKLogger(capture(&buf, slog.LevelDebug)).
		Named("p2pmonitor").
		Warn("relay reset")

	got := lines(t, &buf)
	require.Len(t, got, 1)
	require.Equal(t, "p2pmonitor", got[0]["logger"])
	require.Equal(t, "WARN", got[0]["level"])
}

// With must not let one sublogger's fields leak into a sibling's.
func TestBridgeWithDoesNotShareState(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	base := newSDKLogger(capture(&buf, slog.LevelDebug))

	base.With(zap.String("side", "left")).Info("one")
	base.With(zap.String("side", "right")).Info("two")
	base.Info("three")

	got := lines(t, &buf)
	require.Len(t, got, 3)
	require.Equal(t, "left", got[0]["side"])
	require.Equal(t, "right", got[1]["side"])
	require.NotContains(t, got[2], "side")
}

// The level belongs to the caller's slog handler, so that the debug server can
// change it at runtime. The core itself must not filter.
func TestBridgeLeavesFilteringToSlog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := newSDKLogger(capture(&buf, slog.LevelWarn))

	log.Debug("quiet")
	log.Info("also quiet")
	log.Warn("loud")

	got := lines(t, &buf)
	require.Len(t, got, 1)
	require.Equal(t, "loud", got[0]["msg"])
}
