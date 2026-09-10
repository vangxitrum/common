# retry

Every retry loop in this repository was written by hand, and none of them waits
a random amount of time. That is what this package is for. When a broker or a
database restarts, every instance of every service that was talking to it fails
in the same instant, waits the same second, and reconnects in the same instant.
The dependency least able to absorb a thundering herd is the one that gets it,
and the herd stays synchronized through every doubling.

The second failure is a loop that cannot tell a fault worth retrying from one
that is not. A malformed request retried four times is four times the log noise
and none of the recovery; a connection reset returned on the first attempt is an
outage the caller could have survived.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/retry"
```

## Usage

Bounded work, an error return:

```go
backoff := retry.New(cfg.Retry, retry.WithRetryable(isTransient), retry.WithLogger(log))

err := backoff.Do(ctx, func(ctx context.Context) error {
    return publishOnce(ctx, message)
})
```

A supervisor that runs for the life of the process and starts its wait over
after each success:

```go
loop := backoff.Loop()
for loop.Wait(ctx) {
    connected, err := conn.runOnce(ctx)
    if connected {
        loop.Reset()
    }
    if err != nil {
        log.Warn("connection lost; reconnecting", "attempt", loop.Attempt(), "err", err)
    }
}
return loop.Err()
```

Metrics need no wiring: `retry_attempts`, `retry_elapsed` and `retry_waits`
reach `monkit.Default` on their own. There is no `Chain` call, because there is
nothing to poll - see below.

## Full jitter halves your average wait, on purpose

Each wait is drawn uniformly from zero to its nominal value, so the *expected*
wait is half the nominal one. `MaxDelay: 30s` means a mean wait of fifteen
seconds, not thirty.

That is the price of not having every instance wake at the same moment, and it
is worth paying. The alternative usually proposed is equal jitter -
`d/2 + rand(d/2)` - which keeps half of each wait deterministic, so the wake-up
window is only half as wide and the herd stays half clustered at every doubling.
Full jitter is also what AWS measured as producing both the least total work and
the shortest completion time.

`MaxDelay` bounds *one* wait. `MaxElapsed` is the field to use when you need a
guarantee about total time.

Turning `Jitter` off makes each wait exactly its nominal value. Do that in a
test, not in production.

## The context decides whether you were canceled, not the error

An inner per-attempt timeout and an outer deadline both produce
`context.DeadlineExceeded`. They mean opposite things - the inner one is a
fault worth retrying, the outer one is the caller saying stop - and no amount of
error inspection can tell them apart. Only `ctx.Err()` can, so that is what this
package asks.

The corollary catches people out: an error that matches `context.Canceled` while
`ctx.Err()` is nil came from *somebody else's* context - a canceled sibling, a
pool's internal context - and is an ordinary fault, subject to the classifier
like any other. Treating it as "we were told to stop" is a real bug in
hand-rolled loops, and this package does not make it.

## A classifier is policy; `Permanent` is an override

`WithRetryable` takes a predicate, because what is worth retrying is a property
of a subsystem rather than of one call site. `rabbitmq` already has exactly one
such predicate over three loops. A predicate also keeps `retry` out of the
imports of every leaf that can fail: a function five frames down returning a
plain `*net.OpError` is classified correctly without knowing this package
exists.

`Permanent(err)` is the override for the case a predicate structurally cannot
see - code that knows *this* failure is its own doing. It is checked first, and
it survives wrapping:

```go
return fmt.Errorf("building the request: %w", retry.Permanent(err))
```

With no classifier, every error is retried. That is deliberate: `Do` has to do
something, the least surprising reading of a call to a package named `retry` is
that it retries, and over-retrying is already bounded by `MaxAttempts`. A
default of retrying nothing would be a package that looks like it works and
silently never retries.

## When the other end names the wait, it wins

`Loop.Override` makes the next wait a duration you were given rather than one
the backoff computed - an HTTP `Retry-After`, a broker's own backpressure. The
attempt and elapsed budgets still apply, and the computed sequence is untouched,
so a later failure that carries no instruction picks up where it left off.

Bounding the value is the caller's job.
[`httpclient`](../httpclient/README.md) refuses a `Retry-After` longer than its
maximum for that reason: sleeping as long as a remote asks is a way for whoever
operates that remote to hold your goroutines.

## The last real error survives the deadline

When the budget runs out, the returned error wraps the last one your function
produced, so `errors.Is` and `errors.As` still reach it:

```
retry: giving up after 4 attempts in 7.31s: dial tcp 10.0.0.4:5432: connection refused
```

The same holds when a deadline cuts the sequence short. If there is not enough
time left for another attempt, the loop stops *before* spending the remainder on
a wait that cannot be followed by one - and hands back `connection refused`
rather than a bare `context deadline exceeded`. That is the difference between a
page someone can act on and one they cannot.

## Tests advance time instead of spending it

Waits go through [`time2`](../time2/README.md), so a test drives them with a
`Machine`:

```go
ctx, machine := time2.WithNewMachine(ctx)
...
machine.BlockThenAdvance(ctx, 1, time.Second)
```

Pass `retry.WithRandom(func() float64 { return 1 })` to make each wait exactly
its nominal value, which is what makes an exact sequence assertable. Bound a
test's loop with `MaxElapsed` or a cancellation rather than a context deadline:
`ctx.Deadline` is wall clock, and mixing it with a machine clock proves nothing.

## What this package does not do

It is not a circuit breaker: there is no shared state across calls, so a
dependency that is down is discovered again by every caller. It is not a rate
limiter. It does not impose a per-attempt timeout - that is
`context.WithTimeout` inside your function, where the budget belongs to the
attempt rather than to the sequence.

It implements no `monkit.StatSource` and needs no `Chain` call. Every number it
has is pushed at the moment it happens; `redis.Client.Stats` exists only because
a pool gauge does not exist unless someone asks for it.

## API

```go
type Config struct {
	Name         string
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	MaxElapsed   time.Duration
	Multiplier   float64
	Jitter       bool
}

const (
	DefaultInitialDelay = 200 * time.Millisecond
	DefaultMaxDelay     = 30 * time.Second
	DefaultMultiplier   = 2.0
)

type Option func(*Backoff)

func WithRetryable(fn func(error) bool) Option
func WithRandom(fn func() float64) Option
func WithLogger(log *slog.Logger) Option

type Backoff struct{ ... }

func New(config Config, opts ...Option) *Backoff
func (b *Backoff) Do(ctx context.Context, fn func(ctx context.Context) error) error
func (b *Backoff) Loop() *Loop

type Loop struct{ ... }

var ErrExhausted = errors.New("retry budget exhausted")

func (l *Loop) Wait(ctx context.Context) bool
func (l *Loop) Reset()
func (l *Loop) Override(d time.Duration)
func (l *Loop) Attempt() int
func (l *Loop) Elapsed(ctx context.Context) time.Duration
func (l *Loop) Err() error

func Permanent(err error) error
func IsPermanent(err error) bool

var Error = errs.Tag("retry")
```

## Tests

```bash
go test ./retry/...
```

No docker, no network, and no wall-clock waiting beyond a few milliseconds: the
suite runs in well under a second. If it starts taking thirty, a `time.Sleep`
has crept in where a `time2` call belongs.
