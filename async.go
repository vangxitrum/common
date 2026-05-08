package logger

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

type asyncEntry struct {
	ctx context.Context
	r   slog.Record
}

// asyncHandler wraps any slog.Handler and dispatches records via a buffered
// channel so callers never block on slow sinks. Records are dropped (not
// blocked) when the channel is full.
type asyncHandler struct {
	inner   slog.Handler
	ch      chan asyncEntry
	wg      *sync.WaitGroup
	stopped *uint32
	dropped *uint64
}

func newAsyncHandler(inner slog.Handler, bufSize int) *asyncHandler {
	h := &asyncHandler{
		inner:   inner,
		ch:      make(chan asyncEntry, bufSize),
		wg:      new(sync.WaitGroup),
		stopped: new(uint32),
		dropped: new(uint64),
	}
	h.wg.Add(1)
	go h.run()
	return h
}

func (h *asyncHandler) run() {
	defer h.wg.Done()
	for e := range h.ch {
		_ = h.inner.Handle(e.ctx, e.r)
	}
}

func (h *asyncHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle enqueues r for asynchronous processing. Returns immediately.
// If the channel is full, the record is silently dropped and Dropped() increments.
func (h *asyncHandler) Handle(ctx context.Context, r slog.Record) error {
	if atomic.LoadUint32(h.stopped) == 1 {
		return nil
	}
	clone := r.Clone()
	select {
	case h.ch <- asyncEntry{ctx: ctx, r: clone}:
	default:
		atomic.AddUint64(h.dropped, 1)
	}
	return nil
}

func (h *asyncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &asyncHandler{
		inner:   h.inner.WithAttrs(attrs),
		ch:      h.ch,
		wg:      h.wg,
		stopped: h.stopped,
		dropped: h.dropped,
	}
}

func (h *asyncHandler) WithGroup(name string) slog.Handler {
	return &asyncHandler{
		inner:   h.inner.WithGroup(name),
		ch:      h.ch,
		wg:      h.wg,
		stopped: h.stopped,
		dropped: h.dropped,
	}
}

// shutdown stops accepting new records, drains the queue, and waits for the
// consumer goroutine to finish. Blocks until ctx expires or drain completes.
func (h *asyncHandler) shutdown(ctx context.Context) error {
	atomic.StoreUint32(h.stopped, 1)
	close(h.ch)
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

// Dropped returns the count of records dropped due to a full buffer.
func (h *asyncHandler) Dropped() uint64 {
	return atomic.LoadUint64(h.dropped)
}
