package logger

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"
)

// LevelCritical is one severity above Error. It maps to syslog "critical" (2)
// in Graylog. Use Critical or CriticalContext to emit at this level; they
// automatically attach a full stack trace as the "stacktrace" attribute.
const LevelCritical = slog.Level(slog.LevelError + 4)

// Critical logs at LevelCritical on log and attaches an automatic stack trace.
func Critical(log *slog.Logger, msg string, args ...any) {
	criticalInner(context.Background(), log, msg, args)
}

// CriticalContext logs at LevelCritical on log with context-injected fields
// and an automatic stack trace.
func CriticalContext(ctx context.Context, log *slog.Logger, msg string, args ...any) {
	criticalInner(ctx, log, msg, args)
}

func criticalInner(ctx context.Context, log *slog.Logger, msg string, args []any) {
	if !log.Enabled(ctx, LevelCritical) {
		return
	}

	// Call stack:
	//   runtime.Callers           (skip 0)
	//   criticalInner             (skip 1)
	//   Critical / CriticalContext (skip 2)
	//   user code                 (skip 3) ← we want this
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])

	r := slog.NewRecord(time.Now(), LevelCritical, msg, pcs[0])
	r.Add(args...)
	r.Add(slog.String("stacktrace", captureStack(3)))

	_ = log.Handler().Handle(ctx, r)
}

// Stack returns a formatted stack trace starting from the caller's frame.
// Use it to manually attach a trace to any log level:
//
//	slog.Error("unexpected state", "stacktrace", logger.Stack())
func Stack() string {
	return captureStack(2)
}

// captureStack returns a formatted stack trace, skipping skip frames above
// captureStack itself. Frames from the Go runtime internals are omitted.
func captureStack(skip int) string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(skip+1, pcs)
	if n == 0 {
		return ""
	}
	frames := runtime.CallersFrames(pcs[:n])
	var sb strings.Builder
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "runtime.") {
			fmt.Fprintf(&sb, "\n%s\n\t%s:%d", f.Function, f.File, f.Line)
		}
		if !more {
			break
		}
	}
	return sb.String()
}
