package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const timeFormat = "[2006-01-02 15:04:05.000]"

// Sink is a log destination beyond stdout (file, remote, etc.).
type Sink interface {
	Handle(level slog.Level, t time.Time, msg string, attrs map[string]any) error
	Close() error
}

type consoleHandler struct {
	level     slog.Leveler
	format    Format
	addSource bool
	sinks     []Sink
	out       io.Writer

	mu       *sync.Mutex // shared across clones to serialize stdout writes
	preAttrs []slog.Attr
	groups   []string

	// replace is Config.ReplaceAttr, applied to every attribute before it
	// reaches stdout or a sink.
	replace func(groups []string, a slog.Attr) slog.Attr
}

func newConsoleHandler(level slog.Leveler, format Format, addSource bool, sinks []Sink) *consoleHandler {
	return &consoleHandler{
		level:     level,
		format:    format,
		addSource: addSource,
		sinks:     sinks,
		out:       os.Stdout,
		mu:        new(sync.Mutex),
	}
}

func (h *consoleHandler) clone() *consoleHandler {
	c := *h
	c.preAttrs = make([]slog.Attr, len(h.preAttrs))
	copy(c.preAttrs, h.preAttrs)
	c.groups = make([]string, len(h.groups))
	copy(c.groups, h.groups)
	return &c
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := h.clone()
	if len(h.groups) > 0 {
		// nest attrs under the active group path
		c.preAttrs = append(c.preAttrs, slog.Group(strings.Join(h.groups, "."), attrsToAny(attrs)...))
	} else {
		c.preAttrs = append(c.preAttrs, attrs...)
	}
	return c
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := h.clone()
	c.groups = append(c.groups, name)
	return c
}

func (h *consoleHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make(map[string]any, len(h.preAttrs)+r.NumAttrs()+4)

	// All three sources go through collect, so ReplaceAttr sees every
	// attribute: a handler wrapped around this one could not, because the
	// context attributes are only read here.
	h.collect(attrs, h.preAttrs, "", nil)

	if ctxAttrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		h.collect(attrs, ctxAttrs, "", nil)
	}

	prefix := ""
	var groups []string
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
		groups = h.groups
	}
	r.Attrs(func(a slog.Attr) bool {
		h.collectAttr(attrs, a, prefix, groups)
		return true
	})

	var src string
	if h.addSource && r.PC != 0 {
		frames := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := frames.Next()
		src = fmt.Sprintf("%s:%d", f.File, f.Line)
	}

	h.mu.Lock()
	h.writeConsole(r.Level, r.Time, r.Message, src, attrs)
	h.mu.Unlock()

	attrs["logged_at"] = r.Time
	for _, s := range h.sinks {
		_ = s.Handle(r.Level, r.Time, r.Message, attrs)
	}

	return nil
}

func (h *consoleHandler) writeConsole(level slog.Level, t time.Time, msg, src string, attrs map[string]any) {
	if h.format == FormatJSON {
		m := make(map[string]any, len(attrs)+3)
		for k, v := range attrs {
			m[k] = v
		}
		m["time"] = t.Format(time.RFC3339Nano)
		m["level"] = levelStr(level)
		m["msg"] = msg
		if src != "" {
			m["source"] = src
		}
		b, _ := json.Marshal(m)
		_, _ = fmt.Fprintln(h.out, string(b))
		return
	}

	// colorized text
	var sb strings.Builder
	sb.WriteString(colorize(lightGray, t.Format(timeFormat)))
	sb.WriteByte(' ')
	sb.WriteString(levelColor(level))
	sb.WriteByte(' ')
	sb.WriteString(colorize(white, msg))
	if src != "" {
		sb.WriteByte(' ')
		sb.WriteString(colorize(darkGray, "src="+src))
	}
	var stack string
	for k, v := range attrs {
		if k == "logged_at" {
			continue
		}
		if k == "stacktrace" {
			stack, _ = v.(string)
			continue
		}
		sb.WriteByte(' ')
		sb.WriteString(colorize(darkGray, fmt.Sprintf("%s=%v", k, v)))
	}
	if stack != "" {
		sb.WriteString(colorize(lightRed, "\nstacktrace:"+stack))
	}
	_, _ = fmt.Fprintln(h.out, sb.String())
}

func levelStr(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "DBG"
	case slog.LevelInfo:
		return "INF"
	case slog.LevelWarn:
		return "WRN"
	case slog.LevelError:
		return "ERR"
	case LevelCritical:
		return "CRT"
	default:
		return level.String()
	}
}

func levelColor(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return colorize(darkGray, "DBG")
	case slog.LevelInfo:
		return colorize(cyan, "INF")
	case slog.LevelWarn:
		return colorize(lightYellow, "WRN")
	case slog.LevelError:
		return colorize(lightRed, "ERR")
	case LevelCritical:
		return colorizeBold(lightRed, "CRT")
	default:
		return colorize(lightGray, level.String())
	}
}

// collect flattens attrs into dst under prefix. groups is the same path as a
// slice, for ReplaceAttr.
func (h *consoleHandler) collect(dst map[string]any, attrs []slog.Attr, prefix string, groups []string) {
	for _, a := range attrs {
		h.collectAttr(dst, a, prefix, groups)
	}
}

func (h *consoleHandler) collectAttr(dst map[string]any, a slog.Attr, prefix string, groups []string) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		// The three-index slice makes append copy, so sibling groups never
		// share a backing array.
		h.collect(dst, a.Value.Group(), prefix+a.Key+".", append(groups[:len(groups):len(groups)], a.Key))
		return
	}
	if h.replace != nil {
		if a = h.replace(groups, a); a.Key == "" {
			return
		}
		a.Value = a.Value.Resolve()
	}
	dst[prefix+a.Key] = a.Value.Any()
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}

// ANSI color constants and helper
const (
	reset = "\033[0m"

	cyan        = 36
	lightGray   = 37
	darkGray    = 90
	lightRed    = 91
	lightYellow = 93
	white       = 97
)

func colorize(code int, s string) string {
	return "\033[" + strconv.Itoa(code) + "m" + s + reset
}

func colorizeBold(code int, s string) string {
	return "\033[1;" + strconv.Itoa(code) + "m" + s + reset
}
