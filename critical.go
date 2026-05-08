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
// in Graylog. The Critical and CriticalContext helpers automatically attach a
// full stack trace as the "stacktrace" attribute.
const LevelCritical = slog.Level(slog.LevelError + 4)

// Critical logs at LevelCritical and attaches an automatic stack trace.
func (l *Logger) Critical(msg string, args ...any) {
	l.logCritical(context.Background(), msg, args)
}

// CriticalContext logs at LevelCritical with context-injected fields and an
// automatic stack trace.
func (l *Logger) CriticalContext(ctx context.Context, msg string, args ...any) {
	l.logCritical(ctx, msg, args)
}

func (l *Logger) logCritical(ctx context.Context, msg string, args []any) {
	if !l.Logger.Enabled(ctx, LevelCritical) {
		return
	}

	// Capture the caller's PC for source attribution.
	// Call stack at this point:
	//   runtime.Callers (skip 0)
	//   logCritical     (skip 1)
	//   Critical / CriticalContext (skip 2)
	//   user code       (skip 3) ← we want this
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])

	r := slog.NewRecord(time.Now(), LevelCritical, msg, pcs[0])
	r.Add(args...)
	// Stack trace starts from user code (3 frames above captureStack).
	r.Add(slog.String("stacktrace", captureStack(3)))

	_ = l.Logger.Handler().Handle(ctx, r)
}

// Stack returns a formatted stack trace starting from the caller's frame.
// Use it to manually attach a trace to any log level:
//
//	log.Error("unexpected state", "stacktrace", logger.Stack())
func Stack() string {
	// Call stack: runtime.Callers(0) / captureStack(1) / Stack(2) / user(3)
	return captureStack(2)
}

// captureStack returns a formatted stack trace, skipping skip frames above
// captureStack itself. Frames from the Go runtime internals are omitted.
//
// The resulting string has the form:
//
//	\npkg.Function\n\t/path/to/file.go:line
//	\npkg.Function\n\t/path/to/file.go:line
//	...
func captureStack(skip int) string {
	pcs := make([]uintptr, 64)
	// +1 so that captureStack's own frame is always skipped regardless of caller.
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
