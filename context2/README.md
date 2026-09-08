# context2

Three context shapes the standard library does not provide.

`context` gives you cancellation that propagates down and a `Done` you cannot
explain. These three cover the cases where that is not what you want: work that
must outlive the request that started it, a deadline that has to be extended
after the fact, and a cancellation that says why.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/context2"
```

## Usage

### Work that must not die with its request

```go
// The cleanup still needs the request's values (trace ids, auth), but it must
// not be killed when the client hangs up.
go cleanup(context2.WithoutCancellation(ctx))
```

The returned context keeps the parent's values and drops its `Done` and its
deadline. That is exactly why it is dangerous: nothing will ever cancel it, so
give the work its own timeout or it leaks.

### A deadline decided after the context was made

```go
ctx, cancel := context2.WithRetimeout(ctx, 30*time.Second)
defer cancel()
```

`WithRetimeout` gives a fresh timeout to a context that may already be
canceled, which `context.WithTimeout` cannot do: it inherits the parent's
cancellation, so a canceled parent yields an already-dead child.

### A cancellation that carries a reason

```go
sub, cancel := context2.WithCustomCancel(ctx)
...
cancel(io.ErrUnexpectedEOF)
<-sub.Done()
sub.Err() // io.ErrUnexpectedEOF, not context.Canceled
```

`context.Canceled` tells a caller that something gave up, and nothing else. The
place that decides to cancel usually knows why, and `WithCustomCancel` is how
that reason reaches the place that reports the failure. Cancelling with the
parent still surfaces the parent's error.

## API

```go
func WithoutCancellation(ctx context.Context) context.Context
func WithRetimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc)
func WithCustomCancel(ctx context.Context) (context.Context, func(error))
```
