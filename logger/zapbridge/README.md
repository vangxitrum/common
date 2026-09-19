# zapbridge

Joins a zap logger and an `slog.Handler`, in both directions.

It is for a service whose call sites are zap - because it was written that way,
or because a library it embeds offers no other hook - and whose process logging
is this module's handler.
Rewriting the call sites is not the price of the shared handler.

```bash
go get gitlab.internal/tuan.quang.tran/aioz-common/logger/zapbridge
```

## Forward: zap lines into the handler

`Tee` adds the handler as a second destination on a zap logger, leaving the
console output that logger already has exactly as it is.

```go
handler, _, err := logging.New(cfg.LogSink)
if err != nil {
    return err
}
log = zapbridge.Tee(log, handler, zapcore.DebugLevel)
```

Tee it onto the **root** logger, as early as the sink's configuration is known.
Every logger derived later with `Named` or `With` inherits the bridge, and the
lines written while the process is still starting are usually the ones worth
shipping.

`Core` is the same thing one level down, for a caller assembling its own
`zapcore.NewTee` - a process that also ships to Loki, say, wants one tee with
three cores rather than two wraps.

**The level.** Enable the Core at `zapcore.DebugLevel` and let zap's own
`AtomicLevel` decide, so a debug server that raises the level at runtime
reaches these lines too. The handler is asked as well, so a handler built at
`info` silently drops what zap has just allowed through - build it at the most
verbose level the process can be switched to, and leave the filtering to zap.

## What crosses

| zap | slog |
| --- | --- |
| `entry.Time` | the record's time, not the time it was handled |
| `entry.Caller` | the record's PC, so `AddSource` names the real call site |
| `entry.LoggerName` | attribute `logger` |
| `entry.Stack` | attribute `stacktrace` |
| `zap.Error(err)` | attribute holding **the error**, not its message |
| everything else | through `Field.AddTo`, so every zap type encodes as zap encodes it |

The error matters more than it looks. This module's handler takes the error
value to derive `error.stack_trace`; an error flattened to a string on the way
across leaves that field empty on every document.

## Reverse: a slog-typed package, a zap process

`Handler` wraps a zap logger as an `slog.Handler`, for handing this module's
packages - `lifecycle`, `stats/debug`, `stats/telemetry` - a logger when the
process's own is zap.

```go
group := lifecycle.NewGroup(slog.New(zapbridge.Handler(log)))
```

Their lines then take the same console format, the same level and the same
sinks as the rest of the process, instead of arriving in a second format from
a second handler.

Use it over the process's zap logger only. Over a zap logger teed into a
handler with `Core`, records go out through that handler once, as everything
else does; over a zap logger built *on* a handler, the two feed each other.

Groups flatten to dotted keys, matching what `logger` does with them. The
record's time is kept. Its caller is not: zap decides a caller from the stack,
and the stack there is this adapter.

## Why a package of its own

`logger` is imported by every service in the fleet, and most of them have never
seen zap. Keeping the bridge here means zap enters a module graph only when a
consumer asks for it, and it is the one place in this repository the
`enforce-slog` lint rule is waived.
