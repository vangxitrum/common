# time2

A clock you can pass around, and a time machine for tests.

Code that calls `time.Now`, `time.NewTimer` or `time.Sleep` directly can only
be tested by waiting. That makes the test slow when it passes and flaky when it
does not, and a test for a 24-hour expiry cannot be written at all. `time2`
routes those calls through a `Clock` carried on the context, so a test can
advance time instead of spending it.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/time2"
```

## Usage

Production code takes a context and asks it for the time:

```go
func (c *Cache) expired(ctx context.Context, entry Entry) bool {
    return time2.Since(ctx, entry.Added) > c.ttl
}

func (w *Worker) backoff(ctx context.Context, d time.Duration) bool {
    return time2.Sleep(ctx, d) // false if the context ended first
}
```

With no clock on the context these are the real `time` functions, so nothing is
paid in production and no wiring is required at any call site that does not
want it.

A test installs a machine and moves the clock itself:

```go
machine, ctx := time2.WithNewMachine(ctx)

go cache.Run(ctx)

machine.BlockThenAdvance(ctx, 1, time.Hour) // wait for one timer, then jump
```

## The trap: advance and you race, block and you do not

`Advance` fires every timer that the jump expires. If the code under test has
not created its timer yet, the jump passes it by, the timer is then scheduled
in the new present, and the test hangs or, worse, passes for the wrong reason.

`Block(ctx, n)` waits until `n` timers are scheduled. `BlockThenAdvance` does
both in the right order, and it is what a test should almost always call. It is
not safe to call concurrently: the machine is a single clock, and driving it
from two goroutines reintroduces the nondeterminism it exists to remove.

## API

- **On a context**: `WithClock`, `Now`, `Since`, `Sleep`.
- **`Clock`** (zero value is the real clock): `Now`, `Since`, `Sleep`,
  `NewTimer`, `NewTicker`.
- **`Timer` and `Ticker`**: the same shape as the `time` types, so the calling
  code does not change.
- **`Machine`**: `NewMachine`, `WithNewMachine`, `Clock`, `Now`, `Advance`,
  `Block`, `BlockThenAdvance`.
