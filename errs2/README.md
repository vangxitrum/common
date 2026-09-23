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

## Where an error began

```go
frames := errs2.Frames(err)
// ["user.(*Service).DeleteExpiredRefreshTokens:254", "chore.(*Chore).Run.func1:81", ...]
```

`zeebo/errs` captures a stack on its first wrap but keeps it private: it only
prints it under `%+v`, glued to the message as `\n\t`-separated frames, and
only when the errs error is the outermost one. Wrap it in `fmt.Errorf("%w")`
or an `apierr` and `%+v` prints no stack at all. `Frames` walks the chain and
returns the frames as a slice, and `Origin` returns the error they came from.

`Origin` is the **innermost** errs error, because its stack starts where the
failure did. An errs error wrapped again after a `fmt.Errorf` captures a new
stack that starts higher up. It is found by type, not with `errs.Tags`, which
reports nothing for `errs.Errorf` and `errs.Wrap`: they capture a stack under
an empty tag.

## API

```go
func IsCanceled(err error) bool
func IgnoreCanceled(err error) error
func Collect(errch chan error, duration time.Duration) error
func Origin(err error) error
func Frames(err error) []string

type Group struct{ ... }
func (group *Group) Go(f func() error)
func (group *Group) Wait() []error
```
