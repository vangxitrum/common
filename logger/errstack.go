package logger

import (
	"log/slog"
	"strings"

	"gitlab.internal/tuan.quang.tran/aioz-common/errs2"
)

// stackKey is the attribute the console prints as a block below the line.
const stackKey = "stacktrace"

// plumbing are the frames that say how a goroutine was started rather than
// what failed. lifecyclePkg is matched anywhere in a frame, since the host and
// owner in front of it vary by checkout.
var plumbing = []string{"runtime.", "runtime/", "golang.org/x/sync/errgroup."}

const lifecyclePkg = "/aioz-common/lifecycle."

// ErrorStack returns where err began, one "function:line" per frame, trimmed
// for reading: the host and owner are dropped from each import path, and the
// runtime, errgroup and lifecycle frames that start every goroutine are left
// out. It returns nil when nothing in err's chain carries a stack; see
// [errs2.Frames].
func ErrorStack(err error) []string {
	var out []string
	for _, frame := range errs2.Frames(err) {
		if isPlumbing(frame) {
			continue
		}
		out = append(out, trimFrame(frame))
	}
	return out
}

// FormatStack renders frames as the block the console prints under a line:
// "\n  stack:" and then each frame on its own indented line. It returns ""
// for no frames.
func FormatStack(frames []string) string { return formatBlock("stack", frames) }

// formatBlock is FormatStack under a chosen label, so the error's stack and
// the log statement's call stack print the same shape.
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

// ErrorAttr is a ReplaceAttr that logs an error as its message.
//
// Without it, slog.TextHandler prints an error with %+v, and a zeebo/errs
// error then appends its whole stack, which the handler quotes onto the same
// line with every newline escaped. Pair it with [ErrorStack] and [FormatStack]
// to print the stack where it can be read. The handler in this package does
// both itself and needs no ReplaceAttr for it.
func ErrorAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, err.Error())
		}
	}
	return a
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
