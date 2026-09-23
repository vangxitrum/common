package logger

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceReachesTheSinks(t *testing.T) {
	sink := &recordingSink{}
	log := slog.New(newConsoleHandler(
		slog.LevelDebug, FormatJSON, true, []Sink{sink},
	))

	log.Info("hello")

	require.Len(t, sink.lines, 1)
	src, ok := sink.lines[0]["source"]
	require.True(
		t,
		ok,
		"a sink must receive the call site, or log.origin.file.name is always absent",
	)
	require.Contains(t, src, "source_sink_test.go:",
		"and it must be the caller's file and line")
}

func TestSourceIsAbsentWithoutAddSource(t *testing.T) {
	sink := &recordingSink{}
	log := slog.New(newConsoleHandler(
		slog.LevelDebug, FormatJSON, false, []Sink{sink},
	))

	log.Info("hello")

	require.Len(t, sink.lines, 1)
	_, ok := sink.lines[0]["source"]
	require.False(t, ok, "AddSource off means no source attribute at all")
}

func TestARecordAttributeWinsOverTheCallSite(t *testing.T) {
	// A caller that logs its own "source" is describing something else - the
	// source of a payment, a feed - and must not have it overwritten.
	sink := &recordingSink{}
	log := slog.New(newConsoleHandler(
		slog.LevelDebug, FormatJSON, true, []Sink{sink},
	))

	log.Info("hello", "source", "bank-feed")

	require.Len(t, sink.lines, 1)
	require.Equal(t, "bank-feed", sink.lines[0]["source"])
}
