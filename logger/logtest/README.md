# logtest

Loggers for tests.

```go
log := logtest.New(t) // every record, at debug and above, into t.Log
```

A failing test then prints what the code under test logged; a passing one
prints nothing unless run with `-v`.

```go
log, rec := logtest.NewRecorder()
service := NewService(log)

service.Retry(ctx)

warnings := rec.FilterMessageContains("giving up")
require.Len(t, warnings, 1)
require.Equal(t, "billing", warnings[0].Attrs["component"])
require.ErrorIs(t, warnings[0].Attrs["error"].(error), context.DeadlineExceeded)
```

A `Record` carries the message, the level, the time and every attribute:
`With` attributes included, groups flattened to dotted keys (`piece.id`), and
errors kept as the error value rather than its message, so `errors.Is` still
works on them. `NewRecorderAt(level)` drops what is below `level`, and
`Enabled` reports it, so code that skips building an expensive line when the
level is off is tested as it runs.

To see the lines and assert on them, combine the two:

```go
recorded, rec := logtest.NewRecorder()
log := slog.New(slog.NewMultiHandler(logtest.New(t).Handler(), recorded.Handler()))
```
