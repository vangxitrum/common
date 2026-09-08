# cycle

Runs a function on an interval until it is told to stop.

It exists so a background chore is testable without sleeping: `Trigger` and
`TriggerWait` fire the work on demand, so a test asserts on the result rather
than on a timer. A chore built on a bare `time.Ticker` can only be tested by
waiting for one, which is slow when it passes and flaky when it does not.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/cycle"
```

## Usage

```go
type Chore struct {
    Loop *cycle.Cycle
    log  *slog.Logger
}

func (c *Chore) Run(ctx context.Context) error {
    return c.Loop.Run(ctx, func(ctx context.Context) error {
        if err := c.work(ctx); err != nil {
            // Log a failed pass, do not return it - see below.
            c.log.ErrorContext(ctx, "chore pass failed; retrying next interval",
                "err", err.Error())
            return nil
        }
        return nil
    })
}
```

A test then drives it synchronously, with no timer involved:

```go
chore := NewChore(log, config, service)
go chore.Run(ctx)
chore.Loop.TriggerWait()   // one pass, synchronously
```

## Three rules for the work function

**Log a failed pass, do not return it.** An error from the work stops the cycle,
and the cycle usually runs alongside the servers, so one failed pass takes the
whole service down. A transient fault deserves a log line and the next interval.
Reserve a returned error for a fault that genuinely should stop the service.

**Make it switchable.** An `Enabled` config field costs one line and means a
misbehaving job can be stopped by config rather than by a deploy. A cycle
constructed with a non-positive interval reports `Disabled()` and its `Run`
returns immediately, so nothing waits on it.

**Drive it from tests, do not sleep.** `TriggerWait` runs the work once and
waits for it - the difference between a test that is slow when it passes and
flaky when it does not, and one that is neither.

## API

| | |
| --- | --- |
| `New(interval time.Duration) *Cycle` | a cycle firing every `interval`; non-positive means disabled |
| `(*Cycle) Run(ctx, fn) error` | run until the context is done or `fn` returns an error |
| `(*Cycle) Trigger()` | fire the work now, without waiting |
| `(*Cycle) TriggerWait()` | fire the work now and wait for the pass to finish |
| `(*Cycle) Pause()` / `Resume()` | stop and restart the interval; triggers still work |
| `(*Cycle) SetDelayStart()` | wait one interval before the first pass |
| `(*Cycle) Disabled() bool` | whether the interval is non-positive |
| `(*Cycle) Close()` | release the cycle's resources |
