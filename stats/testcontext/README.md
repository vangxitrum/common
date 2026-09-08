# testcontext

A `context.Context` tied to a test's lifetime, with goroutine tracking.

It exists so a test does not hand-roll the same three things every time: a
context that is canceled when the test ends, a `WaitGroup` around the
goroutines the test starts, and somewhere to put the errors those goroutines
return.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
```

## Usage

```go
func TestChore(t *testing.T) {
    ctx := testcontext.New(t)
    defer ctx.Cleanup()

    ctx.Go(func() error {
        return chore.Run(ctx)
    })

    ...

    ctx.Wait()   // every Go'd function has returned
}
```

`New` registers the cancellation with `t.Cleanup`, so the context is canceled
when the test ends whether or not `Cleanup` is called. Calling `Cleanup`
explicitly waits for the goroutines first, which is usually what you want
before asserting.

## Go reports, it does not fail fast

`Go` records a returned error with `t.Error`, not `t.Fatal`, and that is not a
style choice: `t.Fatal` calls `runtime.Goexit` and is only safe on the test's
own goroutine. Called from a background one it kills the wrong stack and the
test either hangs or reports something unrelated. `Go` reports the error,
attributes it to the test, and lets the test finish.

`Check` is the same idea for a function you are calling inline, typically a
`Close` whose error should fail the test but not abandon it:

```go
defer ctx.Check(server.Close)
```

## API

```go
func New(t *testing.T) *Context

func (c *Context) Go(fn func() error)   // run in a goroutine, report an error
func (c *Context) Wait()                // wait for every Go
func (c *Context) Cleanup()             // Wait, then cancel
func (c *Context) Check(fn func() error)
```

`Context` embeds `context.Context`, so it is passed anywhere one is expected.

## Not a substitute for a clock

For code that waits on time, this is the wrong tool: use
[`time2`](../../time2/README.md) and advance a machine. `testcontext` manages
goroutines and cancellation; it does not make a `time.Sleep` finish sooner.
