// Package retry runs an operation again after a failure, backing off between
// attempts.
//
// Every retry loop in this repository was written by hand, and none of them
// waits a random amount of time. That is the failure this package exists for.
// When a broker or a database restarts, every instance of every service that
// was talking to it fails in the same instant, waits the same second, and
// reconnects in the same instant - so the dependency that is least able to
// absorb a thundering herd is the one that gets it, and the herd stays
// synchronized through every doubling. Full jitter breaks that up.
//
// The second failure is a loop that cannot tell a fault worth retrying from
// one that is not. A malformed request retried four times is four times the log
// noise and none of the recovery; a connection reset returned on the first
// attempt is an outage the caller could have survived.
//
// # Two shapes
//
// [Backoff.Do] is for bounded work that returns an error - a publish, a
// request, a query. [Backoff.Loop] is for a supervisor that runs for the life
// of the process and starts its wait over after each success:
//
//	loop := backoff.Loop()
//	for loop.Wait(ctx) {
//		if connected, _ := conn.runOnce(ctx); connected {
//			loop.Reset()
//		}
//	}
//	return loop.Err()
//
// # The clock
//
// Waits go through [time2], so a test advances time instead of spending it. A
// test that wants an exact wait sequence also passes [WithRandom] returning 1,
// which makes every wait its nominal value.
package retry

import (
	"context"
	"log/slog"

	// math/rand, not crypto/rand, on purpose: jitter only has to differ
	// between instances, not be unguessable. The module's no-math-rand lint
	// rule exempts this file for exactly this use.
	"math/rand/v2"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
)

// Config describes how many times to try and how long to wait between tries.
type Config struct {
	Name string `help:"name of what is being retried; tags this backoff's metrics" default:"retry"`

	MaxAttempts  int           `help:"total attempts including the first; 0 retries until the context ends"                                         default:"4"`
	InitialDelay time.Duration `help:"nominal wait before the second attempt"                                                                       default:"200ms"`
	MaxDelay     time.Duration `help:"upper bound on one wait; 0 uses initial delay, making the wait fixed"                                         default:"30s"`
	MaxElapsed   time.Duration `help:"give up once this much time has passed since the first attempt; 0 disables"                                   default:"0"`
	Multiplier   float64       `help:"factor the nominal wait grows by after each attempt; 1 makes the wait fixed"                                  default:"2"`
	Jitter       bool          `help:"draw each wait uniformly from zero to the nominal wait; turning it off synchronizes every instance's retries" default:"true"`
}

// Defaults used when the config leaves a field at zero.
const (
	DefaultInitialDelay = 200 * time.Millisecond
	DefaultMaxDelay     = 30 * time.Second
	DefaultMultiplier   = 2.0
)

// Option adjusts a Backoff at construction.
type Option func(*Backoff)

// WithRetryable replaces the default policy, which retries every error.
//
// The predicate is the policy for a whole subsystem rather than for one call,
// which is what it already is in practice: rabbitmq has one isBrokerError over
// three call sites. It also keeps the retry package out of the imports of every
// leaf that can fail, which a sentinel-only design cannot.
func WithRetryable(fn func(error) bool) Option {
	return func(b *Backoff) {
		if fn != nil {
			b.retryable = fn
		}
	}
}

// WithRandom replaces the jitter source. It must be safe for concurrent use,
// because one Backoff is shared by every goroutine that retries through it.
//
// A test passes a function returning 1 to make each wait exactly its nominal
// value, which is what makes an exact wait sequence assertable.
func WithRandom(fn func() float64) Option {
	return func(b *Backoff) {
		if fn != nil {
			b.random = fn
		}
	}
}

// WithLogger logs one line per retry at warn level.
func WithLogger(log *slog.Logger) Option {
	return func(b *Backoff) { b.log = log }
}

// Backoff computes the wait between attempts. It is immutable after New and
// safe for concurrent use.
type Backoff struct {
	name monkit.SeriesTag

	maxAttempts  int
	initialDelay time.Duration
	maxDelay     time.Duration
	maxElapsed   time.Duration
	multiplier   float64
	jitter       bool

	retryable func(error) bool
	random    func() float64
	log       *slog.Logger

	attempts *monkit.IntVal
	elapsed  *monkit.DurationVal
	waits    *monkit.Counter
}

// New returns a Backoff.
//
// With no [WithRetryable] the policy is to retry every error. That is the
// deliberate default: Do has to do something, the least surprising reading of a
// call to a package named retry is that it retries, and over-retrying is
// already bounded by MaxAttempts. A default of retrying nothing would be a
// package that looks like it works and silently never retries.
func New(config Config, opts ...Option) *Backoff {
	name := config.Name
	if name == "" {
		name = "retry"
	}

	initialDelay := config.InitialDelay
	if initialDelay <= 0 {
		initialDelay = DefaultInitialDelay
	}

	// A zero maximum means the wait never grows, which is what a caller
	// asking for a fixed delay writes.
	maxDelay := config.MaxDelay
	if maxDelay <= 0 {
		maxDelay = initialDelay
	}
	if maxDelay < initialDelay {
		maxDelay = initialDelay
	}

	multiplier := config.Multiplier
	if multiplier <= 0 {
		multiplier = DefaultMultiplier
	}

	nameTag := monkit.NewSeriesTag("name", name)

	b := &Backoff{
		name: nameTag,

		maxAttempts:  config.MaxAttempts,
		initialDelay: initialDelay,
		maxDelay:     maxDelay,
		maxElapsed:   config.MaxElapsed,
		multiplier:   multiplier,
		jitter:       config.Jitter,

		retryable: func(error) bool { return true },
		// The top-level function of math/rand/v2 keeps per-P state, so it
		// takes no global lock. The v1 top-level functions do, which matters
		// when a hundred goroutines back off at once.
		random: rand.Float64,

		// Created once, here, rather than per call: Scope.IntVal takes a lock
		// and a map lookup, and this is on the path of everything that retries.
		attempts: mon.IntVal("retry_attempts", nameTag),
		elapsed:  mon.DurationVal("retry_elapsed", nameTag),
		waits:    mon.Counter("retry_waits", nameTag),
	}

	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Do calls fn until it succeeds, until the policy says the failure is not worth
// retrying, or until the attempt or time budget runs out.
//
// The error it returns on giving up wraps the last one fn produced, so
// [errors.Is] and [errors.As] still reach the real cause.
func (b *Backoff) Do(
	ctx context.Context,
	fn func(ctx context.Context) error,
) (err error) {
	defer mon.Task()(&ctx)(&err)

	loop := b.Loop()
	var lastErr error

	for loop.Wait(ctx) {
		lastErr = fn(ctx)

		if lastErr == nil {
			b.observe(ctx, loop)
			return nil
		}

		// The context, never the error, decides whether the caller gave up.
		// An inner per-attempt timeout produces the same
		// context.DeadlineExceeded as an outer deadline, and only the context
		// tells them apart - the inner one must be retried, the outer one must
		// not.
		if ctx.Err() != nil {
			b.observe(ctx, loop)
			return lastErr
		}

		if IsPermanent(lastErr) || !b.retryable(lastErr) {
			b.observe(ctx, loop)
			return lastErr
		}

		if b.log != nil {
			b.log.Warn(
				"retrying after a failure",
				"name",
				b.name.Val,
				"attempt",
				loop.Attempt()+1,
				"err",
				lastErr.Error(),
			)
		}
	}

	b.observe(ctx, loop)

	if lastErr == nil {
		// Wait refused the very first attempt, so the context was already done.
		return Error.Wrap(loop.Err())
	}
	if loop.Err() == ErrExhausted {
		mon.Event("retry_exhausted", b.name)
		return Error.Errorf("giving up after %d attempts in %s: %w",
			loop.Attempt()+1, loop.Elapsed(ctx), lastErr)
	}
	return lastErr
}

func (b *Backoff) observe(ctx context.Context, loop *Loop) {
	if !loop.started {
		return
	}
	b.attempts.Observe(int64(loop.Attempt() + 1))
	b.elapsed.Observe(loop.Elapsed(ctx))
}

// wait returns the actual wait for a nominal one.
func (b *Backoff) wait(nominal time.Duration) time.Duration {
	if !b.jitter {
		return nominal
	}
	// Full jitter: uniform on [0, nominal). The expected wait is therefore
	// half the nominal one, which is the cost of not having every instance
	// wake at the same moment. MaxDelay bounds one wait, not the typical wait.
	return time.Duration(b.random() * float64(nominal))
}

// grow returns the next nominal wait, clamped to the maximum.
//
// The clamp is applied to the accumulator rather than computed from an
// exponent, so an unbounded loop - a supervisor with MaxAttempts of zero, which
// runs for days - cannot overflow to +Inf and convert to a garbage duration.
func (b *Backoff) grow(nominal time.Duration) time.Duration {
	next := time.Duration(float64(nominal) * b.multiplier)
	if next > b.maxDelay || next < 0 {
		return b.maxDelay
	}
	return next
}
