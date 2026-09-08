package cycle_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/cycle"
)

// counter records how many times the work ran.
type counter struct{ n atomic.Int64 }

func (c *counter) run(context.Context) error { c.n.Add(1); return nil }

// start runs a cycle in the background and returns a function that stops it
// and reports the error Run returned.
func start(t *testing.T, c *cycle.Cycle, ctx context.Context, fn func(context.Context) error) func() error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, fn) }()

	stopped := false
	t.Cleanup(func() {
		if !stopped {
			c.Close()
		}
	})

	return func() error {
		stopped = true
		c.Close()
		return <-done
	}
}

func TestRunsImmediatelyThenOnTrigger(t *testing.T) {
	t.Parallel()

	var work counter
	// A long interval, so nothing observed here can come from the timer.
	c := cycle.New(time.Hour)
	stop := start(t, c, context.Background(), work.run)

	c.TriggerWait()
	require.EqualValues(t, 2, work.n.Load(), "one immediate run, one triggered")

	c.TriggerWait()
	require.EqualValues(t, 3, work.n.Load())

	require.NoError(t, stop())
}

func TestSetDelayStartSkipsTheFirstRun(t *testing.T) {
	t.Parallel()

	var work counter
	c := cycle.New(time.Hour)
	c.SetDelayStart()
	stop := start(t, c, context.Background(), work.run)

	c.TriggerWait()
	require.EqualValues(t, 1, work.n.Load(), "only the triggered run")

	require.NoError(t, stop())
}

// TestTicks is the one test that waits, because the ticker is the thing under
// test. The interval is small and the assertion is "at least twice", so it
// does not depend on timing precision.
func TestTicks(t *testing.T) {
	t.Parallel()

	var work counter
	c := cycle.New(10 * time.Millisecond)
	stop := start(t, c, context.Background(), work.run)

	require.Eventually(t, func() bool { return work.n.Load() >= 3 },
		2*time.Second, 5*time.Millisecond)

	require.NoError(t, stop())
}

func TestDisabledNeverRuns(t *testing.T) {
	t.Parallel()

	var work counter
	c := cycle.New(0)
	require.True(t, c.Disabled())

	require.NoError(t, c.Run(context.Background(), work.run))
	require.Zero(t, work.n.Load())

	// Close on a cycle whose Run already returned must not hang.
	c.Close()
}

func TestCloseStopsAndIsIdempotent(t *testing.T) {
	t.Parallel()

	var work counter
	c := cycle.New(time.Hour)
	stop := start(t, c, context.Background(), work.run)

	require.NoError(t, stop())

	// A second Close, and one on a never-run cycle, must both be safe.
	c.Close()
	cycle.New(time.Hour).Close()
}

func TestContextCancelStopsTheCycle(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	var work counter
	c := cycle.New(10 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, work.run) }()

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	// Close after the context stopped it must not hang either.
	c.Close()
}

func TestErrorStopsTheCycle(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	c := cycle.New(time.Hour)

	// The immediate first run fails, so Run returns straight away.
	require.ErrorIs(t, c.Run(context.Background(), func(context.Context) error {
		return boom
	}), boom)

	c.Close()
}

// TestTriggerAfterCloseDoesNotBlock: a caller must never hang on a cycle that
// has already stopped.
func TestTriggerAfterCloseDoesNotBlock(t *testing.T) {
	t.Parallel()

	var work counter
	c := cycle.New(time.Hour)
	stop := start(t, c, context.Background(), work.run)
	require.NoError(t, stop())

	completed := make(chan struct{})
	go func() {
		c.Trigger()
		c.TriggerWait()
		c.Pause()
		c.Resume()
		close(completed)
	}()

	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("a control call blocked on a stopped cycle")
	}
}

func TestPauseStopsTheTickerButNotTriggers(t *testing.T) {
	t.Parallel()

	var work counter
	c := cycle.New(10 * time.Millisecond)
	stop := start(t, c, context.Background(), work.run)

	c.Pause()
	settled := work.n.Load()

	// Nothing should tick while paused, but a trigger still runs.
	c.TriggerWait()
	require.EqualValues(t, settled+1, work.n.Load())

	c.Resume()
	require.Eventually(t, func() bool { return work.n.Load() > settled+1 },
		2*time.Second, 5*time.Millisecond)

	require.NoError(t, stop())
}
