// Package logtest provides loggers for tests: one that writes into the test's
// own log, and one that records what was logged so a test can assert on it.
package logtest

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// New returns a logger writing every record, at debug and above, into t's
// log, so a failing test prints what the code under test logged and a passing
// one prints nothing unless run with -v.
func New(t testing.TB) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(tWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type tWriter struct{ t testing.TB }

func (w tWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Record is one logged record, with its attributes flattened: a group's
// members appear under dotted keys, and With attributes are included.
// Values are kept as logged - an error is still an error.
type Record struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// Recorder keeps every record written through the logger it was made with.
// It is safe for concurrent use.
type Recorder struct {
	mu      sync.Mutex
	records []Record
}

// NewRecorder returns a logger recording every record at debug and above,
// and the Recorder to read them back from.
func NewRecorder() (*slog.Logger, *Recorder) {
	return NewRecorderAt(slog.LevelDebug)
}

// NewRecorderAt is NewRecorder with a minimum level.
func NewRecorderAt(level slog.Leveler) (*slog.Logger, *Recorder) {
	r := &Recorder{}
	return slog.New(&handler{recorder: r, level: level}), r
}

// All returns every record so far, oldest first.
func (r *Recorder) All() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.records...)
}

// Len returns the number of records so far.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

// FilterMessage returns the records whose message is exactly msg.
func (r *Recorder) FilterMessage(msg string) []Record {
	var out []Record
	for _, rec := range r.All() {
		if rec.Message == msg {
			out = append(out, rec)
		}
	}
	return out
}

// FilterMessageContains returns the records whose message contains s.
func (r *Recorder) FilterMessageContains(s string) []Record {
	var out []Record
	for _, rec := range r.All() {
		if strings.Contains(rec.Message, s) {
			out = append(out, rec)
		}
	}
	return out
}

// FilterLevel returns the records at exactly level.
func (r *Recorder) FilterLevel(level slog.Level) []Record {
	var out []Record
	for _, rec := range r.All() {
		if rec.Level == level {
			out = append(out, rec)
		}
	}
	return out
}

// Reset forgets every record so far.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
}

type handler struct {
	recorder *Recorder
	level    slog.Leveler
	attrs    []slog.Attr
	prefix   string
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		flatten(attrs, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		flatten(attrs, h.prefix, a)
		return true
	})
	h.recorder.mu.Lock()
	defer h.recorder.mu.Unlock()
	h.recorder.records = append(h.recorder.records, Record{
		Time:    r.Time,
		Level:   r.Level,
		Message: r.Message,
		Attrs:   attrs,
	})
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		// Stored with the group prefix applied, since the groups opened
		// after this do not apply to it.
		clone.attrs = append(clone.attrs, prefixed(h.prefix, a))
	}
	return &clone
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.prefix = h.prefix + name + "."
	return &clone
}

func prefixed(prefix string, a slog.Attr) slog.Attr {
	a.Key = prefix + a.Key
	return a
}

func flatten(dst map[string]any, prefix string, a slog.Attr) {
	// An error is kept whole: a LogValuer error would otherwise resolve to
	// its message.
	if err, ok := a.Value.Any().(error); ok {
		dst[prefix+a.Key] = err
		return
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		for _, member := range v.Group() {
			flatten(dst, prefix+a.Key+".", member)
		}
		return
	}
	dst[prefix+a.Key] = v.Any()
}
