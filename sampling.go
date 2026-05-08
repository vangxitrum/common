package logger

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SamplingConfig applies count-based sampling per log level using a sliding
// time window. The first Initial records in each window pass through
// unconditionally. After that, every Thereafter-th record passes through.
// All other records are silently dropped.
//
// Example: Initial=100, Thereafter=10 with Tick=1s means:
//   - The first 100 records per second pass through.
//   - After that, every 10th record passes through.
type SamplingConfig struct {
	// Tick is the window duration. Defaults to 1s.
	Tick time.Duration
	// Initial is the number of records that always pass at the start of each window.
	// Defaults to 100.
	Initial int
	// Thereafter controls sampling after the initial burst: every Nth record passes.
	// 0 means drop all records after the initial burst.
	// Defaults to 100.
	Thereafter int
}

// samplingState is shared across handler copies created by WithAttrs/WithGroup
// so rate limiting is global, not per-child-logger.
type samplingState struct {
	mu       sync.Mutex
	counters map[slog.Level]*samplingCounter
}

type samplingCounter struct {
	mu      sync.Mutex
	count   uint64
	resetAt time.Time
}

type samplingHandler struct {
	inner slog.Handler
	cfg   SamplingConfig
	state *samplingState
}

func newSamplingHandler(inner slog.Handler, cfg SamplingConfig) *samplingHandler {
	if cfg.Tick <= 0 {
		cfg.Tick = time.Second
	}
	if cfg.Initial <= 0 {
		cfg.Initial = 100
	}
	if cfg.Thereafter <= 0 {
		cfg.Thereafter = 100
	}
	return &samplingHandler{
		inner: inner,
		cfg:   cfg,
		state: &samplingState{
			counters: make(map[slog.Level]*samplingCounter),
		},
	}
}

func (h *samplingHandler) counter(level slog.Level) *samplingCounter {
	h.state.mu.Lock()
	c, ok := h.state.counters[level]
	if !ok {
		c = &samplingCounter{}
		h.state.counters[level] = c
	}
	h.state.mu.Unlock()
	return c
}

func (h *samplingHandler) sample(level slog.Level) bool {
	c := h.counter(level)
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if now.After(c.resetAt) {
		c.count = 0
		c.resetAt = now.Add(h.cfg.Tick)
	}
	c.count++

	if c.count <= uint64(h.cfg.Initial) {
		return true
	}
	if h.cfg.Thereafter == 0 {
		return false
	}
	// Pass every Thereafter-th record after the initial burst.
	// count-Initial gives offset from end of initial window.
	// (offset-1) % Thereafter == 0 catches indices 1, 1+T, 1+2T, ...
	excess := c.count - uint64(h.cfg.Initial)
	return (excess-1)%uint64(h.cfg.Thereafter) == 0
}

func (h *samplingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *samplingHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.sample(r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *samplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &samplingHandler{
		inner: h.inner.WithAttrs(attrs),
		cfg:   h.cfg,
		state: h.state,
	}
}

func (h *samplingHandler) WithGroup(name string) slog.Handler {
	return &samplingHandler{
		inner: h.inner.WithGroup(name),
		cfg:   h.cfg,
		state: h.state,
	}
}
