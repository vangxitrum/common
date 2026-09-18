package depin

import (
	"context"
	"log/slog"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// This is the one file in the repo that imports zap, and .golangci.yml's
// enforce-slog rule excludes it by name for that reason. The depin go-sdk
// offers exactly one logging hook, WithLogger(*zap.Logger), so either its logs
// are bridged here or they are thrown away. They are worth keeping: the SDK
// reports its retries, its stalled pieces and its per-transfer summary through
// that logger, and those lines explain a slow download.

// slogCore is a zapcore.Core that writes every entry into a *slog.Logger, so
// the SDK's output joins the process's own log stream with the same handler,
// level and redaction as everything else.
type slogCore struct {
	log   *slog.Logger
	level zapcore.LevelEnabler
	attrs []slog.Attr
}

// newSlogCore returns a Core writing into log, enabled at the levels enab
// allows.
func newSlogCore(log *slog.Logger, enab zapcore.LevelEnabler) zapcore.Core {
	return &slogCore{log: log, level: enab}
}

// newSDKLogger returns the *zap.Logger the SDK is constructed with, writing
// into log.
//
// It lives here rather than at the call site so that this stays the only file
// naming zap, which is what lets the enforce-slog depguard rule be waived for
// one file instead of the whole package. The caller takes the result with :=
// and hands it straight to the SDK, so it never names the type.
//
// The core is enabled at DebugLevel so the caller's slog handler is the single
// place a level is decided. Filtering in both would mean the debug server's
// /logging endpoint could not turn the SDK's lines back on at runtime.
func newSDKLogger(log *slog.Logger) *zap.Logger {
	return zap.New(newSlogCore(log, zapcore.DebugLevel))
}

func (c *slogCore) Enabled(level zapcore.Level) bool {
	return c.level.Enabled(level)
}

func (c *slogCore) With(fields []zapcore.Field) zapcore.Core {
	clone := *c
	clone.attrs = append(c.cloneAttrs(), fieldsToAttrs(fields)...)
	return &clone
}

func (c *slogCore) Check(
	entry zapcore.Entry,
	checked *zapcore.CheckedEntry,
) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *slogCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	attrs := append(c.cloneAttrs(), fieldsToAttrs(fields)...)
	if entry.LoggerName != "" {
		attrs = append(attrs, slog.String("sdk_logger", entry.LoggerName))
	}
	if entry.Stack != "" {
		// The same key the logger package prints as a block under the line.
		attrs = append(attrs, slog.String("stacktrace", entry.Stack))
	}

	// Background, not a request context: a zapcore.Entry carries no context,
	// and the SDK logs from its own goroutines anyway.
	c.log.LogAttrs(
		context.Background(),
		slogLevel(entry.Level),
		entry.Message,
		attrs...,
	)
	return nil
}

// Sync is a no-op: the slog handler owns flushing, and this Core holds nothing.
func (c *slogCore) Sync() error { return nil }

// cloneAttrs copies the accumulated attributes, so sibling Cores made by With
// never share a backing array.
func (c *slogCore) cloneAttrs() []slog.Attr {
	out := make([]slog.Attr, len(c.attrs), len(c.attrs)+4)
	copy(out, c.attrs)
	return out
}

// fieldsToAttrs converts zap fields without enumerating zapcore's field types.
// Field.AddTo knows how to encode every one of them, including the error,
// duration and object types a switch would get subtly wrong, so it does the
// work into a map encoder and the map becomes the attributes.
func fieldsToAttrs(fields []zapcore.Field) []slog.Attr {
	if len(fields) == 0 {
		return nil
	}
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	attrs := make([]slog.Attr, 0, len(enc.Fields))
	for k, v := range enc.Fields {
		attrs = append(attrs, slog.Any(k, v))
	}
	return attrs
}

// slogLevel maps zap's levels onto slog's. zap has three levels above Error
// (DPanic, Panic, Fatal) and slog has none, so they all arrive as Error; the
// SDK does not use them.
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
