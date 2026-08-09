package lrucache

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// handleTracker records opens and closes so a test can assert on the lifecycle
// rather than on internal pool state.
type handleTracker struct {
	mu     sync.Mutex
	opened []string
	closed []string
	live   atomic.Int64
}

func (h *handleTracker) open(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opened = append(h.opened, key)
	h.live.Add(1)
	return "v:" + key
}

func (h *handleTracker) close(key string, _ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = append(h.closed, key)
	h.live.Add(-1)
}

func (h *handleTracker) counts() (opened, closed int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.opened), len(h.closed)
}

func newTrackedPool(
	capacity int,
) (*HandlePool[string, string], *handleTracker) {
	tracker := &handleTracker{}
	return NewHandlePool(capacity, tracker.open, tracker.close), tracker
}

// TestHandlePoolOpensOncePerKey is the caching guarantee. The pool exists
// because opening is expensive - worker/pieces uses it for open file handles -
// so a second Get for a live key must reuse the handle rather than open again.
func TestHandlePoolOpensOncePerKey(t *testing.T) {
	pool, tracker := newTrackedPool(4)

	v1, release1 := pool.Get("a")
	require.Equal(t, "v:a", v1)

	v2, release2 := pool.Get("a")
	require.Equal(t, "v:a", v2)

	opened, closed := tracker.counts()
	require.Equal(t, 1, opened, "the second Get must reuse the open handle")
	require.Zero(t, closed)

	release1()
	release2()
}

// TestHandlePoolEvictsOldestOverCapacity covers the LRU behaviour, including
// that eviction actually closes the handle. A pool that evicted without closing
// would leak a file descriptor per eviction.
func TestHandlePoolEvictsOldestOverCapacity(t *testing.T) {
	pool, tracker := newTrackedPool(2)

	_, releaseA := pool.Get("a")
	releaseA()
	_, releaseB := pool.Get("b")
	releaseB()

	// "a" is now the oldest; adding a third key must evict it.
	_, releaseC := pool.Get("c")
	releaseC()

	tracker.mu.Lock()
	closed := append([]string(nil), tracker.closed...)
	tracker.mu.Unlock()

	require.Equal(
		t, []string{"a"}, closed,
		"the least recently used handle must be closed on eviction",
	)
}

// TestHandlePoolGetRefreshesRecency asserts Get counts as a use. If it did not,
// the pool would evict the handle being used most often.
func TestHandlePoolGetRefreshesRecency(t *testing.T) {
	pool, tracker := newTrackedPool(2)

	_, rA := pool.Get("a")
	rA()
	_, rB := pool.Get("b")
	rB()

	// Touch "a" so "b" becomes the oldest.
	_, rA2 := pool.Get("a")
	rA2()

	_, rC := pool.Get("c")
	rC()

	tracker.mu.Lock()
	closed := append([]string(nil), tracker.closed...)
	tracker.mu.Unlock()

	require.Equal(
		t, []string{"b"}, closed,
		"Get must refresh recency, otherwise the hottest handle is evicted",
	)
}

// TestHandlePoolDoesNotCloseWhileInUse is the reference-counting guarantee, and
// the one with real consequences: a handle evicted while a caller still holds it
// must stay open until that caller releases. Closing early would hand the caller
// a closed file.
func TestHandlePoolDoesNotCloseWhileInUse(t *testing.T) {
	pool, tracker := newTrackedPool(1)

	_, releaseA := pool.Get("a")

	// Force "a" out of the cache while it is still held.
	_, releaseB := pool.Get("b")
	releaseB()

	_, closed := tracker.counts()
	require.Zero(
		t, closed,
		"an evicted handle must not close while a caller still holds it",
	)

	releaseA()

	_, closed = tracker.counts()
	require.Equal(
		t, 1, closed,
		"the handle must close once the last reference is released",
	)
}

// TestHandlePoolDelete covers explicit removal, including that it reports
// whether anything was there.
func TestHandlePoolDelete(t *testing.T) {
	pool, tracker := newTrackedPool(4)

	_, release := pool.Get("a")
	release()

	value, ok := pool.Delete("a")
	require.True(t, ok)
	require.Equal(t, "v:a", value)

	_, closed := tracker.counts()
	require.Equal(t, 1, closed, "Delete must close the handle")

	_, ok = pool.Delete("missing")
	require.False(t, ok, "deleting an absent key reports false")
}

// TestHandlePoolPeek asserts Peek does not open. Callers use it to test for
// presence, so an implementation that opened on miss would turn a cheap check
// into an expensive one.
func TestHandlePoolPeek(t *testing.T) {
	pool, tracker := newTrackedPool(4)

	_, _, ok := pool.Peek("absent")
	require.False(t, ok)

	opened, _ := tracker.counts()
	require.Zero(t, opened, "Peek must not open a missing key")

	_, release := pool.Get("a")
	release()

	value, releasePeek, ok := pool.Peek("a")
	require.True(t, ok)
	require.Equal(t, "v:a", value)
	releasePeek()

	opened, _ = tracker.counts()
	require.Equal(t, 1, opened, "Peek must not reopen a cached key")
}

// TestHandlePoolConcurrentGetOpensOnce covers the documented behaviour that a
// slow Opener runs once even when many callers race for the same key: the
// others wait on it rather than each opening their own.
func TestHandlePoolConcurrentGetOpensOnce(t *testing.T) {
	const callers = 20

	var opens atomic.Int64
	proceed := make(chan struct{})

	pool := NewHandlePool(
		4,
		func(key string) string {
			opens.Add(1)
			<-proceed // hold the open until every caller has arrived
			return "v:" + key
		},
		func(string, string) {},
	)

	var wg sync.WaitGroup
	values := make([]string, callers)
	releases := make([]func(), callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			values[i], releases[i] = pool.Get("shared")
		}()
	}

	close(proceed)
	wg.Wait()

	require.EqualValues(
		t, 1, opens.Load(),
		"concurrent Gets for one key must share a single open",
	)
	for i := range callers {
		require.Equal(t, "v:shared", values[i])
		releases[i]()
	}
}
