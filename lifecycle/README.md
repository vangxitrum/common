# lifecycle

Runs a service's components and stops them again. Every long-lived thing - a
server, a chore - is the same shape: a `Run` that blocks until its context is
done, and a `Close` that stops it. A `Group` holds them by name, starts them
together, and closes them in reverse order.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/lifecycle"
```

## Usage

The composition root registers each component as it is built:

```go
services := lifecycle.NewGroup(log)

services.Add(lifecycle.Item{
    Name:  "api",
    Run:   peer.API.Server.Run,
    Close: peer.API.Server.Close,
})

var runners errgroup.Group
services.Run(ctx, &runners)
err := runners.Wait()
```

Items start together and **close in reverse registration order**, so register a
dependency before the thing that uses it - typically `debug`, then the chores,
then `api`, so the API stops taking requests first and the debug server, which
is what reports on a shutdown, is the last thing to go.

## Why not a bare errgroup

An errgroup and a few `go` statements would start them too. Three things come
with doing it here instead, and each only matters when something is wrong:

- **the components have names.** A failure logs `component stopped
  unexpectedly` with the name, and every goroutine carries a pprof label, so
  `/debug/pprof/goroutine?debug=1` reads `labels: {"component":"debug"}` rather
  than showing anonymous closures;
- **shutdown order is a property of the code**, not of what the author last
  remembered to write;
- **a component that hangs on the way down says so.** After `ShutdownTimeout` -
  15s by default - the group logs which component has not stopped and dumps the
  goroutine stacks. It does not force anything: killing a component mid-write is
  worse than waiting. Without this a hung `Close` hangs the process with nothing
  to say which one it was.

Cancelling the context is how the service is asked to stop, so a component
reporting `ctx.Err()` after cancellation has succeeded. Anything else is a real
failure and brings the rest down with it.

## API

| | |
| --- | --- |
| `NewGroup(log *slog.Logger) *Group` | a group that logs component transitions |
| `(*Group) Add(Item)` | register a component; order determines shutdown order |
| `(*Group) Names() []string` | registered component names, in start order |
| `(*Group) Run(ctx, *errgroup.Group)` | start every component under the errgroup |
| `(*Group) Close() error` | close in reverse order, honouring `ShutdownTimeout` |

An `Item` is `{Name string; Run func(context.Context) error; Close func() error}`.
