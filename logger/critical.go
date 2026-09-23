package logger

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"time"
)

// LevelCritical is one severity above Error. It maps to syslog "critical" (2)
// in Graylog. Use Critical or CriticalContext to emit at this level; they
// automatically attach the call stack as the "callstack" attribute.
//
// It is "callstack" and not "stacktrace" because this is where the process was
// when it logged, not where a failure began; "stacktrace" is the error's own,
// and both can appear on one record. See [Config.StacktraceLevel], which does
// the same for every record at or above a level.
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
	// Captured here rather than in the handler: this is the caller's
	// goroutine, and Handler.Handle leaves a record that already carries a
	// callstack alone.
	if frames := captureCallstack(pcs[0]); len(frames) > 0 {
		r.Add(slog.String(callstackKey, strings.Join(frames, "\n")))
	}

	_ = log.Handler().Handle(ctx, r)
}

// Stack returns the caller's call stack, one trimmed "function:line" per
// frame joined by newlines - the same shape [Config.StacktraceLevel] and
// [Critical] produce. Use it to attach one at any level:
//
//	slog.Error("unexpected state", "callstack", logger.Stack())
//
// The handler leaves a record that already carries a "callstack" alone, so
// this wins over the automatic capture.
func Stack() string {
	// 1 skips Stack itself, leaving its caller innermost.
	return strings.Join(captureCallstackSkip(1), "\n")
}
