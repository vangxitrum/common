# monitor

Wires process and runtime statistics into a monkit registry, so the debug
server's `/metrics` reports how the process itself is doing and not only what
the application counts.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/stats/monitor"
```

## Why it is needed

Without it a freshly started service serves an **empty** `/metrics`: monkit only
knows the series some package has already incremented, which before the first
request is none. An idle service then looks identical to a dead one.

Registering here adds goroutines, memory, GC, rusage, open file descriptors and
uptime from the moment the process starts.

## Usage

The composition root registers before the debug server starts:

```go
monitor.Register(monkit.Default)
```

`Register` is idempotent per registry, because `environment.Register` is not: it
chains its sources again on every call, and registering twice double-counts
every process series.

## Adding your own metrics

Take the package scope and instrument the call:

```go
var mon = monkit.Package()

func (s *Store) Fetch(ctx context.Context, id string) (_ Thing, err error) {
    defer mon.Task()(&ctx)(&err)
    ...
}
```

`monkit.Package()` registers into `monkit.Default`, which is the registry the
[debug server](../README.md) exposes, so the series appears on `/metrics` with
no further wiring.

## API

| | |
| --- | --- |
| `Register(registry *monkit.Registry)` | add process and runtime series; idempotent per registry |
