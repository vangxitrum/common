package logger

import (
	"log/slog"
	"runtime"
	"strconv"
	"strings"
)

// callstackKey is the attribute holding the call stack of the log statement,
// which the console prints as a block below the line.
//
// It is deliberately not [stackKey]. That one is where a failure *began*,
// taken from the error, and the Elasticsearch sink files it under ECS
// error.stack_trace. This one is how the goroutine *reached* the log
// statement, which is not a property of any error, and the two answer
// different questions often enough that neither may displace the other.
const callstackKey = "callstack"

// maxCallstackDepth caps a captured call stack. Past this the frames are the
// goroutine's plumbing, which is dropped anyway.
const maxCallstackDepth = 64

// captureCallstack returns the call stack of the log statement, one trimmed
// "function:line" per frame, innermost first.
//
// pc is the record's own program counter, which slog sets to the call site. It
// is used as the anchor: every frame above it belongs to slog and to this
// package, and their depth is an implementation detail that a fixed skip count
// would encode. When pc is 0, or is not on the stack because a handler was
// invoked directly, the frames are dropped by name instead.
func captureCallstack(pc uintptr) []string { return capture(pc, 1) }

// captureCallstackSkip is captureCallstack with no anchor, for a helper that
// knows its own depth instead: skip is how many frames above the caller of
// this function to drop, so 1 leaves that caller's caller innermost.
func captureCallstackSkip(skip int) []string { return capture(0, skip+1) }

func capture(pc uintptr, skip int) []string {
	pcs := make([]uintptr, maxCallstackDepth)
	n := runtime.Callers(skip+1, pcs)
	if n == 0 {
		return nil
	}
	pcs = pcs[:n]

	// Anchored: everything above the record's own PC is slog's dispatch and
	// this package's handlers, and slicing there needs no knowledge of how
	// deep either is. Unanchored, those frames have to go by name instead,
	// which is also the only case where a caller inside this package would
	// lose its own frame.
	anchored := false
	if pc != 0 {
		for i, got := range pcs {
			if got == pc {
				pcs, anchored = pcs[i:], true
				break
			}
		}
	}

	drop := isLogPlumbing
	if anchored {
		drop = isPlumbing
	}

	out := make([]string, 0, len(pcs))
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		if frame.Function != "" && !drop(frame.Function) {
			out = append(out, trimFrame(
				frame.Function+":"+strconv.Itoa(frame.Line),
			))
		}
		if !more {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// logPlumbing are the frames between the code that logged and the capture:
// slog's own dispatch and this package's handlers. isPlumbing drops the rest.
var logPlumbing = []string{
	"log/slog.",
	"gitlab.internal/tuan.quang.tran/aioz-common/logger.",
}

func isLogPlumbing(function string) bool {
	for _, prefix := range logPlumbing {
		if strings.HasPrefix(function, prefix) {
			return true
		}
	}
	return isPlumbing(function)
}

// hasAttr reports whether r already carries key, so a stack a caller attached
// itself - as Critical does, closer to the call site - is not replaced.
func hasAttr(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = true
			return false
		}
		return true
	})
	return found
}
