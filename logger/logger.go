package logger

import (
	"context"
	"fmt"
	"log/slog"
)

// Format controls how log records are serialized.
type Format int

const (
	FormatText Format = iota // colorized key=value on stdout; plain text to file
	FormatJSON               // JSON objects, one per line
)

// Config is the top-level configuration for New.
type Config struct {
	// Level is the minimum log level. Defaults to Info.
	Level slog.Leveler
	// Format controls output serialization. Defaults to FormatText.
	Format Format
	// AddSource includes caller file:line in every record.
	AddSource bool

	// ReplaceAttr, if set, rewrites or drops each attribute before it is
	// written anywhere: stdout, file or Graylog. It sees attributes from every
	// source - WithAttrs, the context and the record - with their group path,
	// as slog.HandlerOptions.ReplaceAttr does; an Attr with an empty Key is
	// dropped. logging.RedactAttr is the one services use.
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr

	// AsyncBufferSize is the record channel capacity for non-blocking dispatch.
	// 0 is treated as 4096. When full, new records are dropped.
	AsyncBufferSize int

	// Sampling, if set, applies count-based sampling per log level.
	Sampling *SamplingConfig

	// File, if set, enables buffered rotating file output.
	File *FileConfig

	// Graylog, if set, ships logs to a GELF HTTP endpoint.
	Graylog *GraylogConfig
}

// Handler is a slog.Handler that adds async dispatch, buffered file output,
// Graylog shipping, and per-level sampling on top of the standard slog pipeline.
//
// Typical usage:
//
//	h, err := logger.New(cfg)
//	slog.SetDefault(slog.New(h))
//	defer h.Shutdown(ctx)
type Handler struct {
	inner slog.Handler
	async *asyncHandler
	sinks []Sink
}

// New constructs a Handler from cfg. Always call Shutdown before process exit.
func New(cfg Config) (*Handler, error) {
	if cfg.Level == nil {
		cfg.Level = slog.LevelInfo
	}
	if cfg.AsyncBufferSize <= 0 {
		cfg.AsyncBufferSize = 4096
	}

	var sinks []Sink

	if cfg.File != nil {
		fs, err := newFileSink(*cfg.File)
		if err != nil {
			return nil, fmt.Errorf("logger: file sink: %w", err)
		}
		sinks = append(sinks, fs)
	}

	if cfg.Graylog != nil {
		sinks = append(sinks, newGraylogSink(*cfg.Graylog))
	}

	console := newConsoleHandler(cfg.Level, cfg.Format, cfg.AddSource, sinks)
	console.replace = cfg.ReplaceAttr
	var h slog.Handler = console

	if cfg.Sampling != nil {
		h = newSamplingHandler(h, *cfg.Sampling)
	}

	async := newAsyncHandler(h, cfg.AsyncBufferSize)

	return &Handler{
		inner: async,
		async: async,
		sinks: sinks,
	}, nil
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs), async: h.async, sinks: h.sinks}
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), async: h.async, sinks: h.sinks}
}

// Shutdown drains the async queue then flushes and closes all sinks.
// Pass a context with your shutdown deadline.
func (h *Handler) Shutdown(ctx context.Context) error {
	if err := h.async.shutdown(ctx); err != nil {
		return fmt.Errorf("logger: async drain: %w", err)
	}
	for _, s := range h.sinks {
		if err := s.Close(); err != nil {
			return fmt.Errorf("logger: sink close: %w", err)
		}
	}
	return nil
}

// Dropped returns the number of records dropped because the async buffer was full.
func (h *Handler) Dropped() uint64 {
	return h.async.Dropped()
}

// ctxKey is the unexported context key for per-request slog attrs.
type ctxKey struct{}

// WithContext attaches slog.Attrs to ctx. Every log call using that ctx will
// include these attrs automatically. Calling it multiple times accumulates attrs.
func WithContext(ctx context.Context, attrs ...slog.Attr) context.Context {
	if prev, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		merged := make([]slog.Attr, len(prev)+len(attrs))
		copy(merged, prev)
		copy(merged[len(prev):], attrs)
		attrs = merged
	}
	return context.WithValue(ctx, ctxKey{}, attrs)
}

// AppendContext is a convenience wrapper around WithContext that accepts
// alternating key-value pairs (like slog.Info does) instead of slog.Attrs.
//
//	ctx = logger.AppendContext(ctx, "user", userID, "trace", traceID)
//	ctx = logger.AppendContext(ctx, slog.String("key", "val"))  // slog.Attr also accepted
func AppendContext(ctx context.Context, args ...any) context.Context {
	return WithContext(ctx, argsToAttrs(args)...)
}

// argsToAttrs mirrors slog's own key-value parsing: a string key followed by
// any value becomes an Attr; a bare slog.Attr is used as-is; anything else
// becomes an Attr with key "!BADKEY".
func argsToAttrs(args []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(args))
	for len(args) > 0 {
		switch x := args[0].(type) {
		case slog.Attr:
			attrs = append(attrs, x)
			args = args[1:]
		case string:
			if len(args) < 2 {
				attrs = append(attrs, slog.String("!BADKEY", x))
				args = args[1:]
			} else {
				attrs = append(attrs, slog.Any(x, args[1]))
				args = args[2:]
			}
		default:
			attrs = append(attrs, slog.Any("!BADKEY", x))
			args = args[1:]
		}
	}
	return attrs
}
