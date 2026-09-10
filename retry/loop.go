package retry

import (
	"context"
	"errors"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// ErrExhausted is what Loop.Err reports when the attempt or time budget ran
// out. A context cancellation reports the context's own error instead.
var ErrExhausted = errors.New("retry budget exhausted")

// Loop is one retry sequence.
//
// It is not safe for concurrent use: take a Loop per goroutine. The Backoff it
// came from is shared and immutable; the Loop is the part that counts.
type Loop struct {
	b *Backoff

	started bool
	attempt int
	nominal time.Duration
	start   time.Time
	err     error

	override    time.Duration
	hasOverride bool
}

// Loop returns a fresh retry sequence.
func (b *Backoff) Loop() *Loop {
	return &Loop{b: b, nominal: b.initialDelay}
}

// Wait reports whether another attempt should be made.
//
// It returns immediately the first time and sleeps the backoff on every call
// after that, so the loop it drives reads as a plain for-loop and the wait
// happens where the retry does:
//
//	for loop.Wait(ctx) {
//		err = attempt(ctx)
//		...
//	}
//
// It returns false when the context is done, when the attempt budget is used
// up, and when there is not enough time left for another attempt.
func (l *Loop) Wait(ctx context.Context) bool {
	if ctx.Err() != nil {
		l.err = ctx.Err()
		return false
	}

	if !l.started {
		l.started = true
		l.start = time2.Now(ctx)
		return true
	}

	if l.b.maxAttempts > 0 && l.attempt+1 >= l.b.maxAttempts {
		l.err = ErrExhausted
		return false
	}
	if l.b.maxElapsed > 0 && time2.Since(ctx, l.start) >= l.b.maxElapsed {
		l.err = ErrExhausted
		return false
	}

	wait := l.b.wait(l.nominal)
	l.nominal = l.b.grow(l.nominal)

	// A wait the other end asked for replaces the computed one, without
	// disturbing the sequence: if it says nothing next time, the backoff picks
	// up where it left off.
	if l.hasOverride {
		wait, l.hasOverride = l.override, false
	}

	// Do not spend the rest of a deadline on a wait that cannot be followed by
	// an attempt. The caller gets the last real error - connection refused -
	// rather than a bare context deadline exceeded, which is the difference
	// between a page someone can act on and one they cannot.
	//
	// ctx.Deadline is wall clock while the wait below may be on a test clock,
	// so this is only ever an optimization: when the two disagree the sleep
	// still selects on ctx.Done and reports the same thing one wait later.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
		l.err = ErrExhausted
		return false
	}

	l.b.waits.Inc(1)
	if !time2.Sleep(ctx, wait) {
		l.err = ctx.Err()
		return false
	}

	l.attempt++
	return true
}

// Reset starts the wait sequence over, without resetting the attempt count or
// the elapsed clock.
//
// A supervisor calls this after a success: the next outage is a new one, not a
// continuation of the last, and it should not start at the thirty-second wait
// the previous one ended on.
func (l *Loop) Reset() {
	l.nominal = l.b.initialDelay
}

// Override makes the next Wait sleep for d instead of the computed backoff.
//
// It exists for the case where the other end has told you how long to wait -
// an HTTP Retry-After, a broker's own backpressure - which is better
// information than any backoff can compute. The attempt and elapsed budgets
// still apply, and the computed sequence is untouched: if the next failure
// carries no instruction, the backoff picks up where it left off.
//
// The caller decides whether the requested wait is reasonable. A wait taken
// from a remote server without an upper bound is a way for whoever operates
// that server to hold your goroutines.
func (l *Loop) Override(d time.Duration) {
	l.override, l.hasOverride = d, true
}

// Attempt is the zero-based index of the attempt Wait most recently admitted.
// It counts across Reset, so it is the total number of tries this Loop has
// made, less one.
func (l *Loop) Attempt() int { return l.attempt }

// Elapsed is how long it has been since the first attempt.
func (l *Loop) Elapsed(ctx context.Context) time.Duration {
	if !l.started {
		return 0
	}
	return time2.Since(ctx, l.start)
}

// Err reports why Wait returned false: nil if it has not, the context's error
// if the caller stopped us, and ErrExhausted if a budget ran out.
func (l *Loop) Err() error { return l.err }
