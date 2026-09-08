package lifecycle_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"gitlab.internal/tuan.quang.tran/aioz-common/lifecycle"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// blocker is a component that runs until its context is done.
type blocker struct {
	name    string
	started chan struct{}
	closed  bool
	mu      sync.Mutex
}

func newBlocker(name string) *blocker {
	return &blocker{name: name, started: make(chan struct{})}
}

func (b *blocker) Run(ctx context.Context) error {
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}

func (b *blocker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (b *blocker) wasClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func TestRunsEveryItemAndStopsCleanly(t *testing.T) {
	t.Parallel()

	first, second := newBlocker("first"), newBlocker("second")

	group := lifecycle.NewGroup(discard())
	group.Add(lifecycle.Item{Name: "first", Run: first.Run, Close: first.Close})
	group.Add(
		lifecycle.Item{Name: "second", Run: second.Run, Close: second.Close},
	)

	require.Equal(t, []string{"first", "second"}, group.Names())

	ctx, cancel := context.WithCancel(context.Background())
	runners, runCtx := errgroup.WithContext(ctx)
	group.Run(runCtx, runners)

	<-first.started
	<-second.started

	// Canceling is how the service is asked to stop, so every item reporting
	// ctx.Err() is success, not failure.
	cancel()
	require.NoError(t, runners.Wait())

	require.NoError(t, group.Close())
	require.True(t, first.wasClosed())
	require.True(t, second.wasClosed())
}

// TestClosesInReverseOrder is why registration order matters: a dependency is
// registered before its dependent, so it outlives it.
func TestClosesInReverseOrder(t *testing.T) {
	t.Parallel()

	var order []string
	var mu sync.Mutex
	record := func(name string) func() error {
		return func() error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}
	}

	group := lifecycle.NewGroup(discard())
	group.Add(lifecycle.Item{Name: "database", Close: record("database")})
	group.Add(lifecycle.Item{Name: "chore", Close: record("chore")})
	group.Add(lifecycle.Item{Name: "api", Close: record("api")})

	require.NoError(t, group.Close())
	require.Equal(t, []string{"api", "chore", "database"}, order)
}

// TestAnItemsFailureStopsTheRest: one component failing has to bring the
// service down rather than leaving it half-running.
func TestAnItemsFailureStopsTheRest(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	survivor := newBlocker("survivor")

	logs := &lockedBuffer{}
	group := lifecycle.NewGroup(slog.New(slog.NewJSONHandler(logs, nil)))
	group.Add(lifecycle.Item{Name: "survivor", Run: survivor.Run})
	group.Add(lifecycle.Item{
		Name: "failing",
		Run:  func(context.Context) error { return boom },
	})

	runners, runCtx := errgroup.WithContext(context.Background())
	group.Run(runCtx, runners)

	require.ErrorIs(t, runners.Wait(), boom)

	// The failure is logged against the component's name, which is the point
	// of naming them.
	require.Contains(t, logs.String(), "component stopped unexpectedly")
	require.Contains(t, logs.String(), `"component":"failing"`)
}

// TestNilRunAndCloseAreAllowed: a component that only needs stopping - a
// cache, a pool - supplies Close alone, and one that only needs starting
// supplies Run alone.
func TestNilRunAndCloseAreAllowed(t *testing.T) {
	t.Parallel()

	closed := false

	group := lifecycle.NewGroup(discard())
	group.Add(
		lifecycle.Item{
			Name:  "close-only",
			Close: func() error { closed = true; return nil },
		},
	)
	group.Add(
		lifecycle.Item{
			Name: "run-only",
			Run:  func(context.Context) error { return nil },
		},
	)

	runners, runCtx := errgroup.WithContext(context.Background())
	group.Run(runCtx, runners)
	require.NoError(t, runners.Wait())

	require.NoError(t, group.Close())
	require.True(t, closed)
}

// TestCloseCollectsEveryError: one component failing to close must not skip
// the others.
func TestCloseCollectsEveryError(t *testing.T) {
	t.Parallel()

	firstErr, secondErr := errors.New(
		"first failed",
	), errors.New(
		"second failed",
	)
	thirdClosed := false

	group := lifecycle.NewGroup(discard())
	group.Add(
		lifecycle.Item{
			Name:  "third",
			Close: func() error { thirdClosed = true; return nil },
		},
	)
	group.Add(
		lifecycle.Item{
			Name:  "second",
			Close: func() error { return secondErr },
		},
	)
	group.Add(
		lifecycle.Item{Name: "first", Close: func() error { return firstErr }},
	)

	err := group.Close()
	require.Error(t, err)
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
	require.True(t, thirdClosed, "a failing Close must not skip the rest")

	// The message names which component failed.
	require.Contains(t, err.Error(), "first")
	require.Contains(t, err.Error(), "second")
}

// TestSlowShutdownIsReported is the reason this package earns its keep. A
// component that hangs on the way down otherwise hangs the process with
// nothing to say which one it was.
func TestSlowShutdownIsReported(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})

	logs := &lockedBuffer{}
	group := lifecycle.NewGroup(slog.New(slog.NewJSONHandler(logs, nil)))
	group.ShutdownTimeout = 50 * time.Millisecond
	group.Add(lifecycle.Item{
		Name: "sticky",
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			// Ignore the cancellation for a while, the way a component
			// draining a slow connection would.
			<-release
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	runners, runCtx := errgroup.WithContext(ctx)
	group.Run(runCtx, runners)

	<-started
	cancel()

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "component is slow to stop")
	}, 5*time.Second, 10*time.Millisecond)

	require.Contains(t, logs.String(), `"component":"sticky"`)
	// And the stacks, so the hang can be located rather than guessed at.
	require.Contains(t, logs.String(), "goroutine dump for slow shutdown")

	close(release)
	require.NoError(t, runners.Wait())
}

// TestFastShutdownIsNotReported: the watchdog must stay quiet in the normal
// case, or its warning means nothing.
func TestFastShutdownIsNotReported(t *testing.T) {
	t.Parallel()

	prompt := newBlocker("prompt")

	logs := &lockedBuffer{}
	group := lifecycle.NewGroup(slog.New(slog.NewJSONHandler(logs, nil)))
	group.ShutdownTimeout = time.Second
	group.Add(lifecycle.Item{Name: "prompt", Run: prompt.Run})

	ctx, cancel := context.WithCancel(context.Background())
	runners, runCtx := errgroup.WithContext(ctx)
	group.Run(runCtx, runners)

	<-prompt.started
	cancel()
	require.NoError(t, runners.Wait())

	time.Sleep(100 * time.Millisecond)
	require.NotContains(t, logs.String(), "component is slow to stop")
}

// TestEmptyGroupIsUsable: a Peer that failed before registering anything still
// gets Run and Close called on it.
func TestEmptyGroupIsUsable(t *testing.T) {
	t.Parallel()

	group := lifecycle.NewGroup(discard())
	require.Empty(t, group.Names())

	runners, runCtx := errgroup.WithContext(context.Background())
	group.Run(runCtx, runners)
	require.NoError(t, runners.Wait())
	require.NoError(t, group.Close())
}

// lockedBuffer collects log output while the group writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
