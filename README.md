# aioz-logger

A structured logging library for Go built on top of `log/slog`. Provides a consistent log format across stdout, file, and Graylog with non-blocking async dispatch, per-level sampling, and graceful shutdown.

## Features

- **Unified format** — same `[timestamp] LEVEL msg key=value` text format (or JSON) across all outputs
- **Non-blocking** — records are queued on a channel; callers never block on slow sinks
- **Log sampling** — initial burst + every-Nth rate limiting per level per time window
- **Buffered file output** — `bufio.Writer` with periodic flush, configurable buffer size, size-based rotation, and backup retention
- **Graceful shutdown** — drains the async queue, flushes buffers, then closes files
- **Graylog (GELF)** — ships structured logs to a Graylog GELF HTTP input
- **Context-aware fields** — attach attributes to a `context.Context`; they appear on every log call using that context

## Requirements

- Go 1.21+

## Installation

```sh
go get github.com/aioz-network/aioz-logger
```

## Quick start

```go
package main

import (
    "context"
    "log/slog"
    "time"

    logger "github.com/aioz-network/aioz-logger"
)

func main() {
    log, err := logger.New(logger.Config{
        Level:  slog.LevelInfo,
        Format: logger.FormatText,
    })
    if err != nil {
        panic(err)
    }
    defer func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        log.Shutdown(ctx)
    }()

    log.Info("server started", "port", 8080)
    log.Warn("high memory usage", "used_mb", 512)
    log.Error("request failed", "status", 500, "path", "/api/v1/users")
}
```

Output (colorized on terminals):

```
[2026-05-08 12:00:00.000] INF server started port=8080
[2026-05-08 12:00:00.001] WRN high memory usage used_mb=512
[2026-05-08 12:00:00.002] ERR request failed status=500 path=/api/v1/users
```

## Configuration reference

```go
logger.Config{
    // Minimum level to emit. Accepts slog.LevelDebug/Info/Warn/Error
    // or a *slog.LevelVar for runtime level changes.
    Level: slog.LevelInfo,

    // FormatText: colorized "key=value" on stdout, plain text to file.
    // FormatJSON: JSON objects on stdout and to file.
    Format: logger.FormatText,

    // Include caller file:line in every record.
    AddSource: false,

    // Channel capacity for async dispatch. 0 → 4096.
    // Records are dropped (never blocked) when the channel is full.
    AsyncBufferSize: 4096,

    // Optional: rate-limit logs per level.
    Sampling: &logger.SamplingConfig{...},

    // Optional: write logs to a rotating file.
    File: &logger.FileConfig{...},

    // Optional: ship logs to Graylog.
    Graylog: &logger.GraylogConfig{...},
}
```

## Async dispatch

All log calls return immediately. Records are processed by a single background goroutine that calls sinks (file, Graylog) sequentially. When the channel is full, new records are dropped rather than blocking the caller.

```go
log.Info("this returns immediately regardless of sink speed")

// Check how many records were dropped since startup.
fmt.Println("dropped:", log.Dropped())
```

## Log sampling

Sampling limits throughput per log level using a sliding time window. Useful for high-frequency paths (e.g. per-request info logs) where full fidelity is unnecessary.

```go
log, _ := logger.New(logger.Config{
    Level: slog.LevelDebug,
    Sampling: &logger.SamplingConfig{
        Tick:       time.Second, // window size
        Initial:    100,         // first 100 records per window pass unconditionally
        Thereafter: 10,          // after that, every 10th record passes
    },
})
```

Sampling is applied per level independently, so a burst of errors does not consume the info quota.

## File output

```go
log, _ := logger.New(logger.Config{
    File: &logger.FileConfig{
        Path:   "/var/log/myapp/app.log",
        Format: logger.FormatJSON,

        // Write buffer size. bufio flushes automatically when full,
        // bounding peak memory usage to this value. Default: 64 KB.
        BufferSize: 64 * 1024,

        // How often the buffer is flushed to disk. Default: 5s.
        FlushInterval: 5 * time.Second,

        // Rotate when the file reaches this size. Default: 100 MB.
        MaxSize: 100 * 1024 * 1024,

        // Number of rotated backup files to keep (app.log.1, app.log.2, …).
        // 0 = unlimited backups with timestamp suffixes.
        MaxBackups: 7,
    },
})
```

### Rotation behaviour

When the file reaches `MaxSize`:

1. The buffer is flushed and the current file is closed.
2. If `MaxBackups > 0`: `app.log.N` (oldest) is deleted, backups are shifted (`app.log.N-1` → `app.log.N`), then the current file is renamed to `app.log.1`.
3. If `MaxBackups == 0`: the current file is renamed with a UTC timestamp suffix (e.g. `app.log.20260508-120000.000`).
4. A new `app.log` is opened.

### Graceful shutdown

Always call `Shutdown` before exiting to flush buffered data and close files:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := log.Shutdown(ctx); err != nil {
    fmt.Fprintln(os.Stderr, "logger shutdown:", err)
}
```

`Shutdown` drains the async queue first (all queued records are written to sinks), then flushes and closes the file. No records are lost as long as the context does not expire.

## Graylog

```go
log, _ := logger.New(logger.Config{
    Graylog: &logger.GraylogConfig{
        URL:      "http://graylog.internal:12201/gelf",
        Facility: "backend",
        Service:  "payment-service",
        Timeout:  5 * time.Second,
    },
})
```

Records are sent as GELF 1.1 JSON over HTTP. All `slog` attributes are forwarded as additional GELF fields (prefixed with `_`). ANSI color codes are stripped automatically. Graylog HTTP calls run inside the async consumer goroutine, so they never block log callers.

### Custom index fields

By default every log attribute is forwarded to Graylog. Use `IndexFields` to
restrict which attributes are sent — useful for keeping the Graylog index lean
and avoiding high-cardinality fields (e.g. full stack traces, large payloads).

```go
Graylog: &logger.GraylogConfig{
    URL:     "http://graylog.internal:12201/gelf",
    Service: "api-server",
    // Only these attributes are forwarded as GELF additional fields.
    // Everything else (stacktrace, request_body, …) is omitted.
    IndexFields: []string{"trace_id", "user_id", "env", "region", "status"},
},
```

When `IndexFields` is empty (the default), all attributes are forwarded.

## Context-aware fields

Attach fields to a `context.Context` once and they appear on every log call that uses that context — useful for per-request trace IDs:

```go
ctx := logger.WithContext(context.Background(),
    slog.String("trace_id", "abc-123"),
    slog.String("user_id", "u-456"),
)

log.InfoContext(ctx, "payment processed", "amount", 99.99)
// → INF payment processed trace_id=abc-123 user_id=u-456 amount=99.99
```

Fields accumulate; calling `WithContext` again appends to the existing set.

## Full example

```go
package main

import (
    "context"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
    "time"

    logger "github.com/aioz-network/aioz-logger"
)

func main() {
    log, err := logger.New(logger.Config{
        Level:           slog.LevelDebug,
        Format:          logger.FormatText,
        AddSource:       false,
        AsyncBufferSize: 8192,

        Sampling: &logger.SamplingConfig{
            Tick:       time.Second,
            Initial:    200,
            Thereafter: 20,
        },

        File: &logger.FileConfig{
            Path:          "app.log",
            Format:        logger.FormatJSON,
            BufferSize:    128 * 1024,
            FlushInterval: 3 * time.Second,
            MaxSize:       50 * 1024 * 1024,
            MaxBackups:    5,
        },

        Graylog: &logger.GraylogConfig{
            URL:      "http://graylog.internal:12201/gelf",
            Facility: "myapp",
            Service:  "api-server",
            Timeout:  3 * time.Second,
        },
    })
    if err != nil {
        panic(err)
    }

    // Attach request-scoped fields via context.
    ctx := logger.WithContext(context.Background(),
        slog.String("trace_id", "req-001"),
    )

    log.InfoContext(ctx, "server started", "port", 8080)
    log.DebugContext(ctx, "config loaded", "env", "production")

    // Create a child logger with permanent fields.
    dbLog := log.With("component", "database")
    dbLog.Info("connected", "host", "postgres:5432")

    // Wait for signal then shut down cleanly.
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    log.Info("shutting down", "dropped_records", log.Dropped())

    shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := log.Shutdown(shutCtx); err != nil {
        os.Exit(1)
    }
}
```

## Critical logs with stack trace

`LevelCritical` sits one step above `Error`. The `Critical` and `CriticalContext`
helpers capture a full stack trace at the call site and attach it as the
`stacktrace` attribute before the record is queued.

```go
log.Critical("database connection lost", "host", "postgres:5432")
// Console output:
// [2026-05-08 12:00:00.000] CRT database connection lost host=postgres:5432
// stacktrace:
// main.connectDB
//   /app/db.go:87
// main.main
//   /app/main.go:34
```

The `stacktrace` field is forwarded to the file sink and Graylog as a regular
attribute. In Graylog, `LevelCritical` maps to syslog severity **2** (critical),
which triggers alert streams if configured.

For attaching a stack trace to any other log level, use `logger.Stack()`:

```go
log.Error("unexpected nil", "stacktrace", logger.Stack(), "component", "router")
```

## Dynamic log level

Pass a `*slog.LevelVar` to change the minimum level at runtime without restarting:

```go
level := new(slog.LevelVar) // starts at Info
level.Set(slog.LevelDebug)

log, _ := logger.New(logger.Config{Level: level})

// Later, in a handler or admin endpoint:
level.Set(slog.LevelWarn)
```

## Output formats

| Format | Console | File |
|--------|---------|------|
| `FormatText` | Colorized `[timestamp] LEVEL msg key=value …` | Same, no ANSI codes |
| `FormatJSON` | `{"time":"…","level":"…","msg":"…","key":"value"}` | Same |

Both formats use the same key names, so switching between them does not require changes to log consumers.
