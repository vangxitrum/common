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

// Logger wraps *slog.Logger and owns the lifecycle of its backends.
type Logger struct {
	*slog.Logger
	async *asyncHandler
	sinks []Sink
}

// New constructs a Logger from cfg. Always call Shutdown before process exit.
func New(cfg Config) (*Logger, error) {
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

	var h slog.Handler = newConsoleHandler(cfg.Level, cfg.Format, cfg.AddSource, sinks)

	if cfg.Sampling != nil {
		h = newSamplingHandler(h, *cfg.Sampling)
	}

	async := newAsyncHandler(h, cfg.AsyncBufferSize)

	return &Logger{
		Logger: slog.New(async),
		async:  async,
		sinks:  sinks,
	}, nil
}

// Shutdown drains the async queue then flushes and closes all sinks.
// Pass a context with your shutdown deadline.
func (l *Logger) Shutdown(ctx context.Context) error {
	if err := l.async.shutdown(ctx); err != nil {
		return fmt.Errorf("logger: async drain: %w", err)
	}
	for _, s := range l.sinks {
		if err := s.Close(); err != nil {
			return fmt.Errorf("logger: sink close: %w", err)
		}
	}
	return nil
}

// Dropped returns the number of records dropped because the async buffer was full.
func (l *Logger) Dropped() uint64 {
	return l.async.Dropped()
}

// ctxKey is the unexported context key for per-request slog attrs.
type ctxKey struct{}

// WithContext attaches slog.Attrs to ctx. Every log call using that ctx will
// include these attrs automatically.
func WithContext(ctx context.Context, attrs ...slog.Attr) context.Context {
	if prev, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		merged := make([]slog.Attr, len(prev)+len(attrs))
		copy(merged, prev)
		copy(merged[len(prev):], attrs)
		attrs = merged
	}
	return context.WithValue(ctx, ctxKey{}, attrs)
}
