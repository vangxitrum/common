# aioz-logger

A `slog.Handler` for Go that adds async dispatch, buffered file output, Graylog (GELF) and Elasticsearch shipping, and per-level sampling on top of the standard `log/slog` pipeline. No custom logger type — create the handler, pass it to `slog.New` or `slog.SetDefault`, and use the standard `slog` API everywhere.

## Features

- **Unified format** — same `[timestamp] LEVEL msg key=value` text format (or JSON) across all outputs
- **Non-blocking** — records are queued on a channel; callers never block on slow sinks
- **Log sampling** — initial burst + every-Nth rate limiting per level per time window
- **Buffered file output** — `bufio.Writer` with periodic flush, configurable buffer size, size-based rotation, and backup retention
- **Graceful shutdown** — drains the async queue, flushes buffers, then closes files
- **Graylog (GELF)** — ships structured logs to a Graylog GELF HTTP input
- **Elasticsearch** — batches structured logs into a data stream through the bulk API
- **Context-aware fields** — attach attributes to a `context.Context`; they appear on every log call using that context

## Requirements

- Go 1.21+

## Installation

```sh
go get gitlab.internal/tuan.quang.tran/aioz-common
```

## Quick start

```go
package main

import (
    "context"
    "log/slog"
    "time"

    logger "gitlab.internal/tuan.quang.tran/aioz-common/logger"
)

func main() {
    h, err := logger.New(logger.Config{
        Level:  slog.LevelInfo,
        Format: logger.FormatText,
    })
    if err != nil {
        panic(err)
    }
    defer func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        h.Shutdown(ctx)
    }()

    slog.SetDefault(slog.New(h))

    slog.Info("server started", "port", 8080)
    slog.Warn("high memory usage", "used_mb", 512)
    slog.Error("request failed", "status", 500, "path", "/api/v1/users")
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

    // Optional: rewrite or drop each attribute before any output sees it.
    // Applied to attributes from With, the context and the record alike,
    // with their group path; an empty Key drops the attribute.
    // logging.New installs logging.RedactAttr here.
    ReplaceAttr: logging.RedactAttr(),

    // Channel capacity for async dispatch. 0 → 4096.
    // Records are dropped (never blocked) when the channel is full.
    AsyncBufferSize: 4096,

    // Optional: rate-limit logs per level.
    Sampling: &logger.SamplingConfig{...},

    // Optional: write logs to a rotating file.
    File: &logger.FileConfig{...},

    // Optional: ship logs to Graylog.
    Graylog: &logger.GraylogConfig{...},

    // Optional: ship logs to Elasticsearch.
    Elasticsearch: &logger.ElasticsearchConfig{...},
}
```

## Handler API

`Handler` implements `slog.Handler` and additionally exposes:

| Method | Description |
|--------|-------------|
| `Shutdown(ctx) error` | Drains the async queue, flushes and closes all sinks |
| `Dropped() uint64` | Count of records dropped due to a full async buffer |

Keep the `*Handler` reference for lifecycle management; use `*slog.Logger` (from `slog.New(h)`) for logging.

## Async dispatch

All log calls return immediately. Records are processed by a single background goroutine that calls sinks (file, Graylog, Elasticsearch) sequentially. When the channel is full, new records are dropped rather than blocking the caller.

```go
slog.Info("this returns immediately regardless of sink speed")

fmt.Println("dropped:", h.Dropped())
```

## Log sampling

Sampling limits throughput per log level using a sliding time window. Useful for high-frequency paths (e.g. per-request info logs) where full fidelity is unnecessary.

```go
h, _ := logger.New(logger.Config{
    Level: slog.LevelDebug,
    Sampling: &logger.SamplingConfig{
        Tick:       time.Second, // window size
        Initial:    100,         // first 100 records per window pass unconditionally
        Thereafter: 10,          // after that, every 10th record passes
    },
})
```

Sampling is applied per level independently — a burst of errors does not consume the info quota.

### Per-level overrides

Use `Levels` to apply different parameters to specific log levels. Any zero field in a level entry inherits the global value.

```go
Sampling: &logger.SamplingConfig{
    Tick:       time.Second,
    Initial:    100,
    Thereafter: 100,

    Levels: map[slog.Level]logger.LevelSamplingConfig{
        slog.LevelDebug: {Initial: 10, Thereafter: 5},    // aggressive: 10/s then every 5th
        slog.LevelWarn:  {Tick: 5 * time.Second, Initial: 500}, // wider window for warnings
        slog.LevelError: {Initial: 10_000, Thereafter: 1}, // near-unlimited for errors
    },
},
```

## File output

```go
h, _ := logger.New(logger.Config{
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

### Rotation behavior

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
if err := h.Shutdown(ctx); err != nil {
    fmt.Fprintln(os.Stderr, "logger shutdown:", err)
}
```

`Shutdown` drains the async queue first (all queued records are written to sinks), then flushes and closes the file. No records are lost as long as the context does not expire.

## Graylog

```go
h, _ := logger.New(logger.Config{
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

By default every log attribute is forwarded to Graylog. Use `IndexFields` to restrict which attributes are sent — useful for keeping the Graylog index lean and avoiding high-cardinality fields (e.g. full stack traces, large payloads).

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

## Elasticsearch

```go
logger.Config{
    Elasticsearch: &logger.ElasticsearchConfig{
        URL:         "https://logs.example.com", // base URL; /_bulk is appended
        DataStream:  "logs-aioz-backend",
        BearerToken: "...",       // or APIKey, or Username/Password
        Service:     "backend",   // service.name on every document
        Role:        "api",       // service.role on every document
        Host:        "vps-1",     // host.name on every document

        // The attributes worth a permanent mapping entry. Everything else
        // still ships, under log.extra, unmapped.
        IndexFields: []string{"service", "role", "env", "region"},
    },
}
```

Documents are ECS-shaped, so a Kibana data view over `logs-aioz-*` works
without configuring fields: `@timestamp`, `message`, `log.level`
(`DEBUG`/`INFO`/`WARN`/`ERROR`/`CRITICAL`), `log.origin.file.name` from
`AddSource`, `error.message` and `error.stack_trace` from an `error` attribute
and `logger.Critical`, and `trace.id` from a `trace_id` attribute. ANSI
coloring is stripped.

### A service declares what it searches by

The record's attributes are flattened into key-value pairs before they reach a
sink - a group logs as `outer.inner`, a `LogValuer` is resolved, `ReplaceAttr`
has already run - and the sink then has to decide where each pair goes.

`IndexFields` is that decision. A declared attribute is written where
Elasticsearch maps it, as `labels.<name>` or, for a name with an ECS home, that
field. Everything else is written to `log.extra`, which the index template maps
with `"enabled": false`.

```
{
  "@timestamp": "...",
  "message": "upload failed",
  "service": { "name": "backend", "role": "api" },
  "labels":  { "env": "prod" },            <- declared: keyword, filterable
  "log": { "level": "ERROR", "extra": {    <- everything else: stored, unmapped
      "attempt": 3,
      "part_number": 7
  }}
}
```

Both halves are shipped. The difference is the mapping: a field Elasticsearch
maps is a field that stays in the mapping for ever, and an index has a field
limit - a thousand by default - that a service inventing attribute names at
runtime will eventually reach. `log.extra` is in `_source`, so it is there in
Discover and in an API response; it just cannot be filtered or aggregated on.

`IndexFields` empty takes `logger.DefaultIndexFields`, which is `service` and
`role`. Both name a dimension a dashboard splits by rather than a detail of one
event, which is what earns a permanent mapping entry. `env`, `region` and
`group` are the usual additions. A request id is usually not one: there is one
value per request, and correlation is what `trace.id` is for.

Four attributes are always written where ECS puts them, declared or not:
`source`, `error`, the stack trace, and `trace_id`. They describe the event
rather than a dimension of it, and a stack trace filed under an unmapped object
is a stack trace nobody can search for.

`Service`, `Role` and `Host` are stamped on every document from the config. A
record attribute of the same name wins, because the call site knew something
the process-wide setting did not. `Role` is fixed for the process - `api`,
`worker`, `job-runner` - so that a service deployed as several of them can be
told apart; it is config rather than a per-record attribute because a value
that can be forgotten at a call site will be.

This is not `GraylogConfig.IndexFields`, which drops what it does not list.

Unlike the Graylog sink, which POSTs one record at a time, this one batches:
`BatchSize` records (default 500) or `FlushInterval` (default 5s), whichever
comes first, and `Close` flushes what is left. Batching matters because the
sink runs on the single async consumer goroutine, so a per-record round trip
would show up as a rising `Handler.Dropped()` rather than as slow logging.

A rejected batch is reported and dropped, not retried - buffering for an
Elasticsearch that is down is the failure mode that must not take the service
with it.

Sending the bearer token rather than a password is the point of the
`BearerToken` field: a reverse proxy in front of Elasticsearch checks the token
and swaps in the real credentials, so no Elasticsearch password is deployed
alongside the service.

## Context-aware fields

Attach fields to a `context.Context` once and they appear on every log call that uses that context — useful for per-request trace IDs.

### WithContext

Accepts `slog.Attr` values explicitly:

```go
ctx := logger.WithContext(context.Background(),
    slog.String("trace_id", "abc-123"),
    slog.String("user_id", "u-456"),
)

slog.InfoContext(ctx, "payment processed", "amount", 99.99)
// → INF payment processed trace_id=abc-123 user_id=u-456 amount=99.99
```

### AppendContext

A convenience wrapper that accepts alternating key-value pairs (the same style as `slog.Info`):

```go
ctx = logger.AppendContext(ctx, "trace_id", "abc-123", "user_id", "u-456")

// slog.Attr values are also accepted inline:
ctx = logger.AppendContext(ctx, "trace_id", "abc-123", slog.Int("attempt", 3))
```

Both functions accumulate — calling either multiple times appends to the existing set rather than replacing it:

```go
ctx = logger.AppendContext(ctx, "trace_id", traceID)
ctx = logger.AppendContext(ctx, "user_id", userID)
// both fields appear on every subsequent log call using ctx
```

## Critical logs with stack trace

`LevelCritical` sits one step above `Error`. `Critical` and `CriticalContext` capture a full stack trace at the call site and attach it as the `stacktrace` attribute before the record is queued.

```go
logger.Critical(slog.Default(), "database connection lost", "host", "postgres:5432")
// Console output:
// [2026-05-08 12:00:00.000] CRT database connection lost host=postgres:5432
//   stack:
//     main.connectDB
//     	/app/db.go:87
//     main.main
//     	/app/main.go:34

// With a child logger and context:
ctx := context.Background()
dbLog := slog.Default().With("component", "database")
logger.CriticalContext(ctx, dbLog, "connection lost")
```

The `stacktrace` field is forwarded to the file sink and Graylog as a regular attribute. In Graylog, `LevelCritical` maps to syslog severity **2** (critical), which triggers alert streams if configured.

`logger.LevelCritical` is a regular `slog.Level` constant, so it works with the standard slog API too:

```go
slog.Log(ctx, logger.LevelCritical, "manual critical", "key", "val")
slog.Default().Enabled(ctx, logger.LevelCritical)
```

For attaching a stack trace to any other log level, use `logger.Stack()`:

```go
slog.Error("unexpected nil", "stacktrace", logger.Stack(), "component", "router")
```

## Errors and their stacks

Log an error as it is. The handler writes its message, and when something in
its chain was made by [zeebo/errs](https://github.com/zeebo/errs), the stack of
where it began goes in `stacktrace`, which the console prints as a block under
the line:

```go
slog.Error("chore pass failed; retrying next interval", "err", err)
```

```
[2026-09-14 11:04:40.603] ERR chore pass failed; retrying next interval chore=user:delete-expired-refresh-tokens err=user: ERROR: relation "refresh_tokens" does not exist (SQLSTATE 42P01)
  stack:
    vms-v2/internal/app/user.(*Service).DeleteExpiredRefreshTokens:254
    vms-v2/internal/utils/chore.(*Chore).Run.func1:81
    aioz-common/cycle.(*Cycle).Run:84
    vms-v2/internal/utils/chore.(*Chore).Run:76
```

- JSON, the file sink and Graylog get `err` as a string and `stacktrace` as
  one `function:line` per line.
- The frames are trimmed. The host and owner are dropped from each import path
  (`10.0.0.50/tuan.quang.tran/vms-v2/...` becomes `vms-v2/...`), and the
  runtime, errgroup and lifecycle frames that start every goroutine are left
  out.
- The stack is found through `fmt.Errorf("%w")` and through an `apierr`,
  whose cause's stack is taken before it resolves to its message. See
  [`errs2.Frames`](../errs2/README.md).
- A record gets the stack of its first error. `Critical`'s call-site stack
  wins over it.

### In a handler built on slog.TextHandler

`slog.TextHandler` prints an error with `%+v`, and a zeebo/errs error then
appends its whole stack, which the handler quotes onto the same line with every
newline escaped:

```
err="user: ERROR: relation \"refresh_tokens\" ...\n\tvms-v2/internal/app/user...:254\n\t..."
```

A handler that wraps one, such as a service's own colorizing handler, can use
the same pieces this package uses:

```go
inner := slog.NewTextHandler(buf, &slog.HandlerOptions{
    // After any other ReplaceAttr, such as logging.RedactAttr().
    ReplaceAttr: logger.ErrorAttr,
})

// In Handle, before the inner handler formats the record, so an apierr is
// still an error rather than its resolved message:
var stack []string
r.Attrs(func(a slog.Attr) bool {
    if err, ok := a.Value.Any().(error); ok && stack == nil {
        stack = logger.ErrorStack(err)
    }
    return true
})

// And when the line is printed:
fmt.Println(line + logger.FormatStack(stack))
```

An error attached with `With` reaches only `ErrorAttr` inside the wrapped
handler, so it is logged as its message with no stack.

| Function | Description |
|----------|-------------|
| `ErrorStack(err) []string` | Where `err` began, trimmed; nil when nothing in its chain carries a stack |
| `FormatStack(frames) string` | The `"\n  stack:"` block the console prints under a line; `""` for none |
| `ErrorAttr` | A `ReplaceAttr` that logs an error value as its message |

## Dynamic log level

Pass a `*slog.LevelVar` to change the minimum level at runtime without restarting:

```go
level := new(slog.LevelVar) // starts at Info
level.Set(slog.LevelDebug)

h, _ := logger.New(logger.Config{Level: level})
slog.SetDefault(slog.New(h))

// Later, in an admin endpoint:
level.Set(slog.LevelWarn)
```

## Output formats

| Format | Console | File |
|--------|---------|------|
| `FormatText` | Colorized `[timestamp] LEVEL msg key=value …` | Same, no ANSI codes |
| `FormatJSON` | `{"time":"…","level":"…","msg":"…","key":"value"}` | Same |

Both formats use the same key names, so switching between them does not require changes to log consumers.

## Integrations

### GORM

Implement GORM's `logger.Interface` backed by a `*slog.Logger`:

```go
import (
    "context"
    "errors"
    "fmt"
    "log/slog"
    "time"

    "gorm.io/gorm"
    gormlogger "gorm.io/gorm/logger"
)

type GormLogger struct {
    log   *slog.Logger
    level gormlogger.LogLevel
}

func NewGormLogger(log *slog.Logger) *GormLogger {
    return &GormLogger{log: log, level: gormlogger.Warn}
}

func (l *GormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
    return &GormLogger{log: l.log, level: level}
}

func (l *GormLogger) Info(ctx context.Context, msg string, args ...any) {
    l.log.InfoContext(ctx, fmt.Sprintf(msg, args...))
}

func (l *GormLogger) Warn(ctx context.Context, msg string, args ...any) {
    l.log.WarnContext(ctx, fmt.Sprintf(msg, args...))
}

func (l *GormLogger) Error(ctx context.Context, msg string, args ...any) {
    l.log.ErrorContext(ctx, fmt.Sprintf(msg, args...))
}

func (l *GormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
    elapsed := time.Since(begin)
    sql, rows := fc()
    attrs := []slog.Attr{
        slog.String("sql", sql),
        slog.Int64("rows", rows),
        slog.Duration("elapsed", elapsed),
    }
    if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
        attrs = append(attrs, slog.Any("error", err))
        l.log.LogAttrs(ctx, slog.LevelError, "query", attrs...)
        return
    }
    l.log.LogAttrs(ctx, slog.LevelDebug, "query", attrs...)
}
```

Usage:

```go
h, _ := logger.New(logger.Config{...})
slog.SetDefault(slog.New(h))

db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
    Logger: NewGormLogger(slog.Default()),
})
```

## Full example

```go
package main

import (
    "context"
    "fmt"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
    "time"

    logger "gitlab.internal/tuan.quang.tran/aioz-common/logger"
)

func main() {
    h, err := logger.New(logger.Config{
        Level:           slog.LevelDebug,
        Format:          logger.FormatText,
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
    slog.SetDefault(slog.New(h))

    // Attach request-scoped fields via context.
    ctx := logger.WithContext(context.Background(),
        slog.String("trace_id", "req-001"),
    )

    slog.InfoContext(ctx, "server started", "port", 8080)
    slog.DebugContext(ctx, "config loaded", "env", "production")

    // Child logger with permanent fields — standard slog pattern.
    dbLog := slog.Default().With("component", "database")
    dbLog.InfoContext(ctx, "connected", "host", "postgres:5432")
    logger.CriticalContext(ctx, dbLog, "connection lost")

    // Wait for signal then shut down cleanly.
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    fmt.Println("dropped records:", h.Dropped())

    shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := h.Shutdown(shutCtx); err != nil {
        os.Exit(1)
    }
}
```
