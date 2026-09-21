package logtest_test

import (
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/logger/logtest"
)

func TestRecorderKeepsRecordsWithTheirAttributes(t *testing.T) {
	t.Parallel()

	log, rec := logtest.NewRecorder()
	cause := errors.New("boom")

	log.With("component", "audit").WithGroup("piece").With("id", "abc").
		Error("failed", "size", 42, "error", cause)

	all := rec.All()
	require.Len(t, all, 1)
	require.Equal(t, slog.LevelError, all[0].Level)
	require.Equal(t, "failed", all[0].Message)
	require.Equal(t, "audit", all[0].Attrs["component"])
	require.Equal(t, "abc", all[0].Attrs["piece.id"])
	require.Equal(t, int64(42), all[0].Attrs["piece.size"])
	require.Equal(t, cause, all[0].Attrs["piece.error"])
}

func TestRecorderFilters(t *testing.T) {
	t.Parallel()

	log, rec := logtest.NewRecorder()
	log.Info("started")
	log.Warn("slow start")
	log.Info("started")

	require.Len(t, rec.FilterMessage("started"), 2)
	require.Len(t, rec.FilterMessageContains("start"), 3)
	require.Len(t, rec.FilterLevel(slog.LevelWarn), 1)
	require.Equal(t, 3, rec.Len())

	rec.Reset()
	require.Zero(t, rec.Len())
}

func TestRecorderAtDropsBelowTheLevel(t *testing.T) {
	t.Parallel()

	log, rec := logtest.NewRecorderAt(slog.LevelWarn)
	log.Info("quiet")
	log.Warn("loud")

	require.Equal(t, 1, rec.Len())
	require.False(t, log.Enabled(t.Context(), slog.LevelInfo))
}

func TestRecorderIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	log, rec := logtest.NewRecorder()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { log.Info("x") })
	}
	wg.Wait()

	require.Equal(t, 20, rec.Len())
}

func TestNewWritesToTheTestLog(t *testing.T) {
	t.Parallel()

	// Nothing to assert beyond not panicking: the output goes to t.Log.
	logtest.New(t).Info("hello", "k", "v")
}
