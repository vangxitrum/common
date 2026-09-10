package retry_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/retry"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// The first attempt happens immediately. A supervisor that waited before its
// first connection would add a second to every process start.
func TestLoopDoesNotWaitBeforeTheFirstAttempt(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	loop := retry.New(testConfig(), noJitter()).Loop()

	require.True(t, loop.Wait(ctx))
	require.Equal(t, 0, loop.Attempt())
	require.NoError(t, loop.Err())
	require.Less(
		t,
		loop.Elapsed(ctx),
		100*time.Millisecond,
		"the first attempt must not wait",
	)
}

// Reset is what makes a supervisor's next outage a new one rather than a
// continuation of the last, which is the behavior rabbitmq's reconnect loop
// documents today.
func TestLoopResetStartsTheWaitSequenceOver(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 0 // a supervisor runs until it is stopped

	var waits []time.Duration
	ctx.Go(func() error {
		loop := retry.New(config, noJitter()).Loop()
		last := time2.Now(clockCtx)

		for i := range 5 {
			if !loop.Wait(clockCtx) {
				break
			}

			now := time2.Now(clockCtx)
			waits = append(waits, now.Sub(last))
			last = now

			// The third attempt is the one that "connected".
			if i == 2 {
				loop.Reset()
			}
		}
		return nil
	})

	for _, wait := range []time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second} {
		require.True(t, machine.BlockThenAdvance(ctx, 1, wait))
	}
	ctx.Wait()

	require.Equal(t, []time.Duration{
		0,               // the first attempt does not wait
		1 * time.Second, // then the sequence doubles
		2 * time.Second,
		1 * time.Second, // Reset puts it back to the start
		2 * time.Second, // and it doubles again from there
	}, waits)
}

// Attempt counts every try this Loop has made, across a Reset, so a log line
// can say how long the outage has been going on.
func TestLoopAttemptCountsAcrossAReset(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	clockCtx, machine := time2.WithNewMachine(ctx)

	config := testConfig()
	config.MaxAttempts = 0

	var attempts []int
	ctx.Go(func() error {
		loop := retry.New(config, noJitter()).Loop()
		for i := 0; loop.Wait(clockCtx) && i < 3; i++ {
			attempts = append(attempts, loop.Attempt())
			loop.Reset()
		}
		return nil
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	ctx.Wait()

	require.Equal(t, []int{0, 1, 2}, attempts)
}

func TestLoopErrReportsWhyItStopped(t *testing.T) {
	t.Parallel()

	t.Run("a used-up attempt budget is exhaustion", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		clockCtx, machine := time2.WithNewMachine(ctx)

		config := testConfig()
		config.MaxAttempts = 2

		var loopErr error
		ctx.Go(func() error {
			loop := retry.New(config, noJitter()).Loop()
			for loop.Wait(clockCtx) { //revive:disable-line:empty-block
			}
			loopErr = loop.Err()
			return nil
		})

		require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
		ctx.Wait()

		require.ErrorIs(t, loopErr, retry.ErrExhausted)
	})

	t.Run("a cancellation is the context's own error", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		canceled, cancel := context.WithCancel(ctx)
		cancel()

		loop := retry.New(testConfig(), noJitter()).Loop()

		require.False(t, loop.Wait(canceled))
		require.ErrorIs(t, loop.Err(), context.Canceled)
	})
}
