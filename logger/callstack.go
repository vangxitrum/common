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
// It is how the goroutine *reached* the log statement, which is a property of
// the log event and not of any error, so the Elasticsearch sink files it under
// ECS log.origin.stack_trace.
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

// plumbing are the frames that say how a goroutine was started rather than
// what it was doing. lifecyclePkg is matched anywhere in a frame, since the host and
// owner in front of it vary by checkout.
var plumbing = []string{"runtime.", "runtime/", "golang.org/x/sync/errgroup."}

const lifecyclePkg = "/aioz-common/lifecycle."

// formatBlock renders frames as the block the console prints under a line:
// "\n  <label>:" and then each frame on its own indented line. It returns ""
// for no frames.
func formatBlock(label string, frames []string) string {
	if len(frames) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n  ")
	sb.WriteString(label)
	sb.WriteString(":")
	for _, frame := range frames {
		sb.WriteString("\n    ")
		sb.WriteString(frame)
	}
	return sb.String()
}

func isPlumbing(frame string) bool {
	for _, prefix := range plumbing {
		if strings.HasPrefix(frame, prefix) {
			return true
		}
	}
	return strings.Contains(frame, lifecyclePkg)
}

// trimFrame drops the host and owner from a frame's import path:
// "10.0.0.50/tuan.quang.tran/vms-v2/internal/app/user.(*Service).Delete:254"
// becomes "vms-v2/internal/app/user.(*Service).Delete:254". A module path is
// recognized by a dot in its first element, as a host has; "main." and
// standard library frames are left alone.
func trimFrame(frame string) string {
	host, rest, ok := strings.Cut(frame, "/")
	if !ok || !strings.Contains(host, ".") {
		return frame
	}
	if _, rest, ok = strings.Cut(rest, "/"); !ok {
		return frame
	}
	return rest
}
