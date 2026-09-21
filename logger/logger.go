package logger

import (
	"context"
	"fmt"
	"io"
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
	// written anywhere: stdout, file, Graylog or Elasticsearch. It sees attributes from every
	// source - WithAttrs, the context and the record - with their group path,
	// as slog.HandlerOptions.ReplaceAttr does; an Attr with an empty Key is
	// dropped. logging.RedactAttr is the one services use.
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr

	// Output is where the console format is written. Defaults to os.Stdout;
	// io.Discard keeps the sinks and silences the console.
	Output io.Writer

	// AsyncBufferSize is the record channel capacity for non-blocking dispatch.
	// 0 is treated as 4096. When full, new records are dropped.
	AsyncBufferSize int

	// Sampling, if set, applies count-based sampling per log level.
	Sampling *SamplingConfig

	// File, if set, enables buffered rotating file output.
	File *FileConfig

	// Graylog, if set, ships logs to a GELF HTTP endpoint.
	Graylog *GraylogConfig

	// Elasticsearch, if set, ships logs to an Elasticsearch data stream
	// through the bulk API.
	Elasticsearch *ElasticsearchConfig

	// ComponentLevels holds a minimum level per component: a record whose
	// "component" attribute names one of these is written only at or above
	// that level, whatever the process level is. It is how one noisy part of a
	// service is quietened without losing the rest of its debug output.
	//
	// The innermost component wins, as it does for a nested named logger: a
	// logger made With("component", "a").With("component", "b") is held to
	// b's level, or to none if b has no entry.
	ComponentLevels map[string]slog.Level
}

// componentKey is the attribute a component is named by. It is the key this
// module's own packages use - lifecycle, stats/debug - so a service that
// follows them gets per-component levels for free.
const componentKey = "component"

// Handler is a slog.Handler that adds async dispatch, buffered file output,
// Graylog and Elasticsearch shipping, and per-level sampling on top of the
// standard slog pipeline.
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

	// levels is Config.ComponentLevels. threshold is the level this logger's
	// component is held to, or nil when it has no component or its component
	// has no entry. inGroup is set once WithGroup has been called: a
	// "component" attribute inside a group names something else.
	levels    map[string]slog.Level
	threshold *slog.Level
	inGroup   bool
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

	if cfg.Elasticsearch != nil {
		sinks = append(sinks, newElasticsearchSink(*cfg.Elasticsearch))
	}

	console := newConsoleHandler(cfg.Level, cfg.Format, cfg.AddSource, sinks)
	if cfg.Output != nil {
		console.out = cfg.Output
	}
	console.replace = cfg.ReplaceAttr
	var h slog.Handler = console

	if cfg.Sampling != nil {
		h = newSamplingHandler(h, *cfg.Sampling)
	}

	async := newAsyncHandler(h, cfg.AsyncBufferSize)

	return &Handler{
		inner:  async,
		async:  async,
		sinks:  sinks,
		levels: cfg.ComponentLevels,
	}, nil
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.threshold != nil && level < *h.threshold {
		return false
	}
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	// A component on the record itself is innermost of all.
	if len(h.levels) > 0 && !h.inGroup {
		var component string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == componentKey {
				component = a.Value.String()
			}
			return true
		})
		if component != "" {
			if min, ok := h.levels[component]; ok && r.Level < min {
				return nil
			}
		}
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.inner = h.inner.WithAttrs(attrs)
	if len(h.levels) > 0 && !h.inGroup {
		for _, a := range attrs {
			if a.Key != componentKey {
				continue
			}
			// The last one wins, and a component with no entry clears the
			// threshold an outer one set.
			clone.threshold = nil
			if min, ok := h.levels[a.Value.String()]; ok {
				clone.threshold = &min
			}
		}
	}
	return &clone
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.inner = h.inner.WithGroup(name)
	if name != "" {
		clone.inGroup = true
	}
	return &clone
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
