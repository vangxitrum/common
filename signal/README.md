# signal

A one-shot error that others can wait on, and a channel that costs nothing
until someone needs it.

Every long-lived component ends up with the same pair of fields: an error
recording why it stopped, and a channel closed when it did. Written by hand
that is a mutex, a `sync.Once`, a nil-check on the channel and a race between
the two. `Signal` is that pair, done once.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/signal"
```

## Usage

```go
type Server struct {
    stopped signal.Signal
}

func (s *Server) Run(ctx context.Context) {
    err := s.serve(ctx)
    s.stopped.Set(err)   // first caller wins; later ones are no-ops
}

func (s *Server) Wait() error {
    s.stopped.Wait()
    return s.stopped.Err()
}

// Or select on it alongside anything else.
select {
case <-s.stopped.Signal():
case <-ctx.Done():
}
```

## Err alone cannot tell you it fired

```go
sig.Set(nil)     // a clean shutdown is still a shutdown
sig.Err()        // nil
sig.IsSet()      // true
err, ok := sig.Get()
```

A `Signal` can legitimately hold a nil error, so a nil `Err()` does **not** mean
the signal is unset. Anywhere the distinction matters, use `IsSet` or `Get`.
`Err() != nil` implies it was set, but not the other way round.

## Chan

`Chan` is the lazy half on its own: a `chan struct{}` that is not allocated
until the first use, for a struct that has many of them and uses few.

```go
var c signal.Chan
c.Send()      // allocates on demand
c.Recv()
c.Close()     // idempotent
```

## API

- **`Signal`**: `Set(err) bool`, `Err() error`, `Get() (error, bool)`,
  `IsSet() bool`, `Wait()`, `Signal() chan struct{}`.
- **`Chan`**: `Get() chan struct{}`, `Make(cap uint)`, `Send()`, `Recv()`,
  `Close()`, `Full() bool`.
