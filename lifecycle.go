// Package lifecycle runs a service's components and stops them again.
//
// Every long-lived thing in this service - the two servers, every chore - is
// the same shape: a Run that blocks until its context is done, and a Close
// that stops it. A Group holds them by name, starts them together, and closes
// them in reverse order.
//
// Doing this by hand works until something goes wrong, and then three things
// are missing. A goroutine dump shows anonymous closures rather than component
// names. Shutdown order is whatever the author last remembered. And a
// component that hangs in Close hangs the process with no clue as to which
// one - the failure this package is most worth having for.
package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs/v2"
	"golang.org/x/sync/errgroup"
)

// Error is the error class for the lifecycle package.
var Error = errs.Tag("lifecycle")

var mon = monkit.Package()

// DefaultShutdownTimeout is how long a component may take to stop before the
// group says so. It does not stop anything: a slow shutdown is reported, never
// forced, because killing a component mid-write is worse than waiting.
const DefaultShutdownTimeout = 15 * time.Second

// Item is one component: something to run, something to close, and a name to
// call it in a log.
//
// Both functions are optional. A component that only needs stopping - a cache,
// a connection pool - supplies Close alone.
type Item struct {
	Name  string
	Run   func(ctx context.Context) error
	Close func() error
}

// Group runs items together and closes them in reverse.
type Group struct {
	log   *slog.Logger
	items []Item

	// ShutdownTimeout bounds how long a component may take to stop before the
	// group logs a warning and dumps the goroutine stacks. Zero means
	// DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// stackOnce keeps one slow component from producing a dump per component.
	stackOnce sync.Once
}

// NewGroup returns an empty Group.
func NewGroup(log *slog.Logger) *Group {
	return &Group{log: log}
}

// Add registers an item. Order matters: items start together but close in
// reverse, so register a dependency before the thing that uses it.
func (group *Group) Add(item Item) {
	group.items = append(group.items, item)
}

// Names lists the registered items in start order.
func (group *Group) Names() []string {
	names := make([]string, 0, len(group.items))
	for _, item := range group.items {
		names = append(names, item.Name)
	}
	return names
}

// Run starts every item in the errgroup and returns immediately. The caller
// waits on the errgroup.
//
// A context cancellation is how this service is asked to stop, so an item that
// reports ctx.Err() after the context was canceled has succeeded, not failed.
// Any other error is logged against the item's name and returned, which
// cancels the group and brings the rest down.
func (group *Group) Run(ctx context.Context, runners *errgroup.Group) {
	defer mon.Task()(&ctx)(nil)

	for _, item := range group.items {
		if item.Run == nil {
			continue
		}

		// Closed when this item's Run returns, so the watchdog below stops
		// watching a component that has already stopped.
		stopped, itemStopped := context.WithCancel(context.Background())
		group.watchShutdown(ctx, stopped, item.Name)

		runners.Go(func() error {
			defer itemStopped()

			var err error
			// A pprof label, so a goroutine dump during a hang names the
			// component instead of showing an anonymous closure.
			pprof.Do(ctx, pprof.Labels("component", item.Name),
				func(ctx context.Context) { err = item.Run(ctx) })

			if errors.Is(ctx.Err(), context.Canceled) {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}

			if err != nil {
				mon.Event("unexpected_shutdown")
				group.log.Error("component stopped unexpectedly",
					"component", item.Name, "err", err.Error())
			}
			return err
		})
	}

	group.log.Debug("components started", "components", group.Names())
}

// watchShutdown reports a component that takes too long to stop.
//
// It cannot stop anything, and deliberately does not try: forcing a component
// down mid-write is worse than waiting for it. What it gives you is the name
// of the component that is hanging, and the stacks to see where.
func (group *Group) watchShutdown(ctx, stopped context.Context, name string) {
	timeout := group.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}

	go func() {
		// Wait for shutdown to begin, unless this item finishes first.
		select {
		case <-ctx.Done():
		case <-stopped.Done():
			return
		}

		deadline := time.NewTimer(timeout)
		defer deadline.Stop()

		select {
		case <-stopped.Done():
		case <-deadline.C:
			mon.Event("slow_shutdown")
			group.log.Warn("component is slow to stop",
				"component", name, "waited", timeout)
			group.logStacks()
		}
	}()
}

// logStacks dumps every goroutine, once per group.
func (group *Group) logStacks() {
	group.stackOnce.Do(func() {
		buf := make([]byte, 64*1024)
		for {
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				buf = buf[:n]
				break
			}
			buf = make([]byte, 2*len(buf))
		}
		group.log.Info("goroutine dump for slow shutdown", "stack", string(buf))
	})
}

// Close closes every item in reverse registration order, collecting errors so
// one failure does not skip the rest.
func (group *Group) Close() error {
	var closeErrors errs.Group

	for i := len(group.items) - 1; i >= 0; i-- {
		item := group.items[i]
		if item.Close == nil {
			continue
		}
		if err := item.Close(); err != nil {
			closeErrors.Add(Error.Errorf("closing %s: %w", item.Name, err))
		}
	}

	return closeErrors.Err()
}
