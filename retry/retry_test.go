package retry_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/retry"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// testLogger routes the package's log lines into the test output, so a failure
// shows what the backoff was doing.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(
		slog.NewTextHandler(
			&testWriter{t: t},
			&slog.HandlerOptions{Level: slog.LevelDebug},
		),
	)
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p))
	return len(p), nil
}

// noJitter makes every wait its nominal value, which is what makes an exact
// wait sequence assertable.
func noJitter() retry.Option { return retry.WithRandom(func() float64 { return 1 }) }

func testConfig() retry.Config {
	return retry.Config{
		Name:         "test",
		MaxAttempts:  4,
		InitialDelay: time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2,
		Jitter:       true,
	}
}

var errBoom = errors.New("boom")

func TestSucceedsOnTheFirstAttempt(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	calls := 0
	err := retry.New(testConfig(), noJitter()).
		Do(ctx, func(context.Context) error {
			calls++
			return nil
		})

	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

// The waits are the whole product, so they are asserted exactly rather than
// approximately: nominal, doubling, clamped at the maximum.
func TestWaitSequenceDoublesAndClamps(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 7
	config.MaxDelay = 10 * time.Second

	var seen []time.Duration
	start := time2.Now(clockCtx)

	ctx.Go(func() error {
		err := retry.New(config, noJitter()).
			Do(clockCtx, func(ctx context.Context) error {
				seen = append(seen, time2.Since(ctx, start))
				return errBoom
			})
		require.ErrorIs(t, err, errBoom)
		return nil
	})

	// One advance per wait: 1s, 2s, 4s, 8s, then the clamp holds it at 10s.
	for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		require.True(t, machine.BlockThenAdvance(ctx, 1, wait))
	}
	ctx.Wait()

	require.Equal(t, []time.Duration{
		0,
		1 * time.Second,
		3 * time.Second,
		7 * time.Second,
		15 * time.Second,
		25 * time.Second,
		35 * time.Second,
	}, seen)
}

// The time machine can only show that a wait was no longer than the advance it
// was given: a backoff that asked for less would still be released by the same
// advance. This one runs on the real clock, with milliseconds instead of
// seconds, to pin the other side - that each wait is at least its nominal
// value. Together the two bound the sequence from both directions.
func TestWaitsAreAtLeastTheNominalDelay(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := retry.Config{
		Name:         "nominal-lower-bound",
		MaxAttempts:  4,
		InitialDelay: 2 * time.Millisecond,
		MaxDelay:     8 * time.Millisecond,
		Multiplier:   2,
		Jitter:       false, // so each wait is exactly its nominal value
	}

	var seen []time.Duration
	start := time.Now()

	err := retry.New(config).Do(ctx, func(context.Context) error {
		seen = append(seen, time.Since(start))
		return errBoom
	})
	require.ErrorIs(t, err, errBoom)

	require.Len(t, seen, 4)
	for i, least := range []time.Duration{2, 4, 8} {
		require.GreaterOrEqual(t, seen[i+1]-seen[i], least*time.Millisecond,
			"wait %d was shorter than its nominal delay", i+1)
	}
}

func TestJitterDrawsFromTheRandomSource(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 2

	ctx.Go(func() error {
		half := retry.WithRandom(func() float64 { return 0.5 })
		err := retry.New(config, half).
			Do(clockCtx, func(context.Context) error { return errBoom })
		require.ErrorIs(t, err, errBoom)
		return nil
	})

	// Half of the one-second nominal wait, and not the nominal wait itself:
	// advancing by less than the draw would leave the sleep pending.
	require.True(t, machine.BlockThenAdvance(ctx, 1, 500*time.Millisecond))
	ctx.Wait()
}

func TestPermanentStopsTheLoop(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	calls := 0
	err := retry.New(testConfig(), noJitter()).
		Do(ctx, func(context.Context) error {
			calls++
			return retry.Permanent(errBoom)
		})

	require.ErrorIs(t, err, errBoom)
	require.True(t, retry.IsPermanent(err))
	require.Equal(t, 1, calls, "a permanent failure is not retried")
}

// The mark has to survive the wrapping every caller does on the way out.
func TestPermanentSurvivesWrapping(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	calls := 0
	err := retry.New(testConfig(), noJitter()).
		Do(ctx, func(context.Context) error {
			calls++
			return retry.Error.Errorf(
				"building the request: %w",
				retry.Permanent(errBoom),
			)
		})

	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 1, calls)
}

func TestPermanentOfNilIsNil(t *testing.T) {
	t.Parallel()

	require.NoError(t, retry.Permanent(nil))
	require.False(t, retry.IsPermanent(nil))
}

func TestClassifierStopsTheLoop(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	calls := 0
	never := retry.WithRetryable(func(error) bool { return false })
	err := retry.New(testConfig(), noJitter(), never).
		Do(ctx, func(context.Context) error {
			calls++
			return errBoom
		})

	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 1, calls)
}

func TestExhaustionWrapsTheLastError(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 3

	ctx.Go(func() error {
		err := retry.New(config, noJitter(), retry.WithLogger(testLogger(t))).
			Do(clockCtx, func(context.Context) error { return errBoom })

		require.ErrorIs(t, err, errBoom, "the real cause must stay reachable")
		require.Contains(t, err.Error(), "giving up after 3 attempts")
		return nil
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, 2*time.Second))
	ctx.Wait()
}

// The headline case. An inner per-attempt timeout and an outer deadline are
// the same context.DeadlineExceeded, and only the context tells them apart.
func TestInnerDeadlineIsRetriedAndOuterIsNot(t *testing.T) {
	t.Parallel()

	t.Run("an inner deadline is a fault and is retried", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		clockCtx, machine := time2.WithNewMachine(ctx)

		config := testConfig()
		config.MaxAttempts = 2

		calls := 0
		ctx.Go(func() error {
			err := retry.New(config, noJitter()).
				Do(clockCtx, func(ctx context.Context) error {
					calls++
					// The attempt's own budget ran out. The caller has not given
					// up, so this is worth another try.
					attempt, cancel := context.WithDeadline(
						ctx,
						time.Now().Add(-time.Second),
					)
					defer cancel()
					return attempt.Err()
				})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			return nil
		})

		require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
		ctx.Wait()

		require.Equal(t, 2, calls, "an inner deadline must not stop the loop")
	})

	t.Run("the caller's deadline stops the loop", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		expired, cancel := context.WithDeadline(
			ctx,
			time.Now().Add(-time.Second),
		)
		defer cancel()

		calls := 0
		err := retry.New(testConfig(), noJitter()).
			Do(expired, func(context.Context) error {
				calls++
				return context.DeadlineExceeded
			})

		require.Error(t, err)
		require.Equal(t, 0, calls, "a context already done never runs the work")
	})
}

// A deadline that cannot fit another attempt should hand back the real error,
// not a bare context error the caller cannot act on.
func TestDeadlineTooShortForAnotherAttemptReturnsTheLastError(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	calls := 0
	err := retry.New(testConfig(), noJitter()).
		Do(deadline, func(context.Context) error {
			calls++
			return errBoom
		})

	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 1, calls, "there was no room for a one-second wait")
}

func TestCancelDuringAWaitReturnsTheLastError(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cancelable, cancel := context.WithCancel(ctx)
	defer cancel()

	clockCtx, machine := time2.WithNewMachine(cancelable)

	ctx.Go(func() error {
		err := retry.New(testConfig(), noJitter()).
			Do(clockCtx, func(context.Context) error { return errBoom })

		require.ErrorIs(
			t,
			err,
			errBoom,
			"a cancellation must not hide why we were retrying",
		)
		return nil
	})

	require.True(t, machine.Block(ctx, 1))
	cancel()
	ctx.Wait()
}

func TestMaxElapsedStopsWithAttemptsRemaining(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 0 // unbounded, so only MaxElapsed can stop this
	config.MaxElapsed = 3 * time.Second

	calls := 0
	ctx.Go(func() error {
		err := retry.New(config, noJitter()).
			Do(clockCtx, func(context.Context) error {
				calls++
				return errBoom
			})
		require.ErrorIs(t, err, errBoom)
		require.Contains(t, err.Error(), "giving up")
		return nil
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, 2*time.Second))
	ctx.Wait()

	require.Equal(t, 3, calls)
}

func TestMultiplierOneGivesAFixedWait(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 3
	config.Multiplier = 1

	ctx.Go(func() error {
		err := retry.New(config, noJitter()).
			Do(clockCtx, func(context.Context) error { return errBoom })
		require.ErrorIs(t, err, errBoom)
		return nil
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	ctx.Wait()
}

// A zero maximum means the wait never grows, which is what a caller asking for
// a fixed delay writes.
func TestZeroMaxDelayFallsBackToTheInitialDelay(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 3
	config.MaxDelay = 0

	ctx.Go(func() error {
		err := retry.New(config, noJitter()).
			Do(clockCtx, func(context.Context) error { return errBoom })
		require.ErrorIs(t, err, errBoom)
		return nil
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	ctx.Wait()
}

func TestMetricsRecordTheAttempts(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.Name = "metrics-record-attempts" // unique, so t.Parallel is safe
	config.MaxAttempts = 3

	ctx.Go(func() error {
		calls := 0
		return retry.New(config, noJitter()).
			Do(clockCtx, func(context.Context) error {
				calls++
				if calls < 3 {
					return errBoom
				}
				return nil
			})
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, 2*time.Second))
	ctx.Wait()

	require.Equal(
		t,
		float64(3),
		seriesValue(t, "retry_attempts", config.Name, "max"),
	)
	require.Equal(
		t,
		float64(2),
		seriesValue(t, "retry_waits", config.Name, "value"),
	)
}

// seriesValue finds one field of the series a backoff of this name publishes.
func seriesValue(t *testing.T, measurement, name, field string) float64 {
	t.Helper()

	var found float64
	var ok bool
	monkit.Default.Stats(func(key monkit.SeriesKey, f string, val float64) {
		if key.Measurement == measurement && key.Tags.Get("name") == name &&
			f == field {
			found, ok = val, true
		}
	})

	require.True(t, ok, "no %s series tagged name=%s", measurement, name)
	return found
}
