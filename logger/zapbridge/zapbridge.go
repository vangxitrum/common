// Package zapbridge joins a zap logger and an slog handler, in both
// directions.
//
// It exists for a service whose call sites are zap - because it was written
// that way, or because a library it embeds offers no other hook - and whose
// process logging is this module's slog handler. Rewriting the call sites is
// not the price of the shared handler: a Core teed onto the zap root sends
// every line the process already writes into the handler, sinks included,
// while the console output it has today is untouched.
//
// It is a package of its own, and not part of logger, so that zap enters a
// consumer's module graph only when that consumer asks for it.
package zapbridge

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// loggerKey is the attribute a zap entry's logger name arrives under. It is
// the component name a process names its loggers with, and the dimension a
// search across a large service starts from.
const loggerKey = "logger"

// stackKey is the attribute a zap entry's stack trace arrives under, the same
// key the logger package prints as a block under the line.
const stackKey = "stacktrace"

// Core returns a zapcore.Core writing every entry it accepts into h.
//
// Tee it onto a logger's own core rather than replacing it: the entry then
// reaches both the console encoder the process configured and the handler,
// and every logger derived later with Named or With inherits the bridge.
//
// enab is usually zapcore.DebugLevel, leaving zap's own level the single place
// a level is decided - filtering in both means a debug server that raises
// zap's level at runtime cannot reach the lines the handler already dropped.
// It then follows that h must be built at its most verbose level, or it
// becomes the filter instead.
func Core(h slog.Handler, enab zapcore.LevelEnabler) zapcore.Core {
	return &core{handler: h, level: enab}
}

// Tee returns log with h added as a second destination, leaving log's own
// encoder and outputs as they are.
func Tee(log *zap.Logger, h slog.Handler, enab zapcore.LevelEnabler) *zap.Logger {
	return log.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, Core(h, enab))
	}))
}

type core struct {
	handler slog.Handler
	level   zapcore.LevelEnabler
	attrs   []slog.Attr
}

func (c *core) Enabled(level zapcore.Level) bool {
	return c.level.Enabled(level)
}

func (c *core) With(fields []zapcore.Field) zapcore.Core {
	clone := *c
	clone.attrs = append(c.cloneAttrs(), fieldsToAttrs(fields)...)
	return &clone
}

func (c *core) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *core) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	attrs := append(c.cloneAttrs(), fieldsToAttrs(fields)...)
	if entry.LoggerName != "" {
		attrs = append(attrs, slog.String(loggerKey, entry.LoggerName))
	}
	if entry.Stack != "" {
		attrs = append(attrs, slog.String(stackKey, entry.Stack))
	}

	// The entry's own time and program counter, not the ones a slog.Logger
	// would stamp here: a handler that buffers - the async one does - would
	// otherwise report when a record was drained, and the caller would be
	// this line.
	// Background, not a request context: a zapcore.Entry carries none, and the
	// goroutine that writes it is often not the one that served the request.
	ctx := context.Background()
	level := slogLevel(entry.Level)
	// The handler is asked too, so a caller that enables this Core at Debug
	// and filters in the handler - which is the arrangement Core documents -
	// still filters.
	if !c.handler.Enabled(ctx, level) {
		return nil
	}

	record := slog.NewRecord(entry.Time, level, entry.Message, entry.Caller.PC)
	record.AddAttrs(attrs...)

	return c.handler.Handle(ctx, record)
}

// Sync is a no-op: the handler owns flushing, and this Core holds nothing.
func (c *core) Sync() error { return nil }

// cloneAttrs copies the accumulated attributes, so sibling Cores made by With
// never share a backing array.
func (c *core) cloneAttrs() []slog.Attr {
	out := make([]slog.Attr, len(c.attrs), len(c.attrs)+4)
	copy(out, c.attrs)
	return out
}

// fieldsToAttrs converts zap fields without enumerating zapcore's field types.
// Field.AddTo knows how to encode every one of them, including the duration
// and object types a switch would get subtly wrong, so it does the work into a
// map encoder and the map becomes the attributes.
//
// An error is the exception: AddTo writes it as its message plus a separate
// errorVerbose, and a handler that derives a stack from the error value - this
// module's does - would find a string. So it is carried through whole, and the
// duplicate message dropped.
func fieldsToAttrs(fields []zapcore.Field) []slog.Attr {
	if len(fields) == 0 {
		return nil
	}

	var attrs []slog.Attr
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		if f.Type == zapcore.ErrorType {
			if err, ok := f.Interface.(error); ok {
				key := f.Key
				if key == "" {
					key = "error"
				}
				attrs = append(attrs, slog.Any(key, err))
				continue
			}
		}
		f.AddTo(enc)
	}
	// zap adds it beside the error it has already been given whole.
	delete(enc.Fields, "errorVerbose")

	for k, v := range enc.Fields {
		attrs = append(attrs, slog.Any(k, v))
	}
	return attrs
}

// slogLevel maps zap's levels onto slog's. zap has three levels above Error
// (DPanic, Panic, Fatal) and slog has none, so they all arrive as Error; the
// entry that carries one is about to stop the process anyway.
func slogLevel(level zapcore.Level) slog.Level {
	switch {
	case level <= zapcore.DebugLevel:
		return slog.LevelDebug
	case level == zapcore.InfoLevel:
		return slog.LevelInfo
	case level == zapcore.WarnLevel:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

// Handler returns an slog.Handler writing every record into log, so a package
// that takes a *slog.Logger can be given a process whose logger is zap.
//
// It is the argument adapter for the other direction, and it belongs over the
// process's own zap logger only. Over a zap logger that is itself teed into an
// slog handler with [Core], records go out through that handler once, as
// everything else does. Over a zap logger built on a handler, the two would
// feed each other.
//
// The record's time is preserved and its caller is not: zap decides a caller
// from the stack, and the stack here is this adapter.
func Handler(log *zap.Logger) slog.Handler {
	return &handler{log: log}
}

type handler struct {
	log    *zap.Logger
	prefix string
	fields []zap.Field
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return h.log.Core().Enabled(zapLevel(level))
}

func (h *handler) Handle(_ context.Context, record slog.Record) error {
	fields := make([]zap.Field, 0, len(h.fields)+record.NumAttrs())
	fields = append(fields, h.fields...)
	record.Attrs(func(a slog.Attr) bool {
		fields = append(fields, attrToFields(h.prefix, a)...)
		return true
	})

	// Written through the Core rather than the Logger so the record keeps the
	// time it was made at.
	entry := zapcore.Entry{
		Level:   zapLevel(record.Level),
		Time:    record.Time,
		Message: record.Message,
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	if checked := h.log.Core().Check(entry, nil); checked != nil {
		checked.Write(fields...)
	}
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.fields = append(append([]zap.Field{}, h.fields...), attrsToFields(h.prefix, attrs)...)
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

func attrsToFields(prefix string, attrs []slog.Attr) []zap.Field {
	fields := make([]zap.Field, 0, len(attrs))
	for _, a := range attrs {
		fields = append(fields, attrToFields(prefix, a)...)
	}
	return fields
}

// attrToFields flattens one attribute, and a group into one field per member
// under a dotted key, which is the shape this module's own handler gives a
// group too.
func attrToFields(prefix string, a slog.Attr) []zap.Field {
	// Taken before Resolve: an error that is also a LogValuer resolves to its
	// message, and the stack behind it would be lost.
	if err, ok := a.Value.Any().(error); ok {
		return []zap.Field{zap.NamedError(prefix+a.Key, err)}
	}
	value := a.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		return attrsToFields(prefix+a.Key+".", value.Group())
	}
	return []zap.Field{zap.Any(prefix+a.Key, value.Any())}
}

// zapLevel maps slog's levels onto zap's. slog's levels are open-ended, so
// anything above Error - the logger package's Critical is - arrives as Error,
// the highest level that does not also stop the process.
func zapLevel(level slog.Level) zapcore.Level {
	switch {
	case level < slog.LevelInfo:
		return zapcore.DebugLevel
	case level < slog.LevelWarn:
		return zapcore.InfoLevel
	case level < slog.LevelError:
		return zapcore.WarnLevel
	default:
		return zapcore.ErrorLevel
	}
}
