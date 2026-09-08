# errs2

Two things `errors` does not do: recognize a cancellation, and collect the
errors from a group of goroutines instead of only the first.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/errs2"
```

## Cancellation is not a failure

```go
if err := chore.Run(ctx); errs2.IgnoreCanceled(err) != nil {
    log.ErrorContext(ctx, "chore failed", "err", err)
}
```

A service that logs at error level every time it shuts down trains everyone to
ignore its error log. `IsCanceled` and `IgnoreCanceled` separate "the context
went away, as designed" from a real fault, including through a wrap chain and
through a combined multi-error.

## Group returns every error, not the first

```go
var group errs2.Group
group.Go(func() error { return a.Run(ctx) })
group.Go(func() error { return b.Run(ctx) })

if errList := group.Wait(); len(errList) > 0 {
    return errs.Combine(errList...)
}
```

`golang.org/x/sync/errgroup` keeps the first error and discards the rest, which
is right when the goroutines are attempts at one thing. It is wrong when they
are separate components starting up: three of them failed for three different
reasons, and being told only about the first turns one debugging session into
three. `Group.Wait` returns them all.

It also does not cancel a shared context on the first failure. The peers keep
running, which is what you want when you are collecting a full picture of what
is broken, and not what you want when the work is speculative - reach for
`errgroup` there.

## Collect

```go
err := errs2.Collect(errch, 5*time.Second)
```

`Collect` waits for the first error on a channel and then keeps gathering for
the given duration before combining. It is for a shutdown path where several
components are failing at once because of the same root cause, and the second
and third errors, arriving milliseconds later, are the ones that explain the
first.

## API

```go
func IsCanceled(err error) bool
func IgnoreCanceled(err error) error
func Collect(errch chan error, duration time.Duration) error

type Group struct{ ... }
func (group *Group) Go(f func() error)
func (group *Group) Wait() []error
```
