package lrucache

import (
	"context"
	"math/rand"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs/v2"
)

func TestCache_LRU(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 2})
	check := newChecker(t, cache)

	check(ctx, "a", 1)
	check(ctx, "a", 1)
	check(ctx, "b", 2)
	check(ctx, "a", 2)
	check(ctx, "c", 3)
	check(ctx, "b", 4)
	check(ctx, "c", 4)
	check(ctx, "a", 5)
}

func TestCache_Expires(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 2, Expiration: time.Second})
	check := newChecker(t, cache)

	now := time.Now()
	check(ctx, "a", 1)

	timeCtx, _ := time2.WithNewMachine(
		ctx,
		time2.WithTimeAt(now.Add(2*time.Second)),
	)
	check(timeCtx, "a", 2)
}

func TestCache_Get_Fuzz(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := New(Options{Capacity: 2, Expiration: 100 * time.Millisecond})
	keys := "abcdefghij"

	var ops uint64
	procs := runtime.GOMAXPROCS(-1)

	for i := 0; i < procs; i++ {
		ctx.Go(func() error {
			rng := rand.New(rand.NewSource(time.Now().UnixNano()))
			for {
				if atomic.AddUint64(&ops, 1) > 1000000 {
					return nil
				}

				shouldErr := rng.Intn(10) == 0
				ran := false
				kidx := rng.Intn(len(keys))
				key := keys[kidx : kidx+1]

				value, err := cache.Get(ctx, key, func() (interface{}, error) {
					ran = true
					if shouldErr {
						return nil, errs.Errorf("random error")
					}
					return key, nil
				})

				if ran {
					if shouldErr && err == nil {
						return errs.Errorf("should have errored and did not")
					}
					if !shouldErr && err != nil {
						return errs.Errorf("should not have errored but did")
					}
				}
				if value != key && (!ran || !shouldErr) {
					return errs.Errorf("expected %q but got %q", key, value)
				}
			}
		})
	}

	ctx.Wait()
}

func TestCache_Get_Dedup(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := New(Options{Capacity: 1})
	fnCalled := make(chan struct{})

	ctx.Go(func() error {
		_, _ = cache.Get(ctx, "key", func() (interface{}, error) {
			fnCalled <- struct{}{}
			time.Sleep(time.Millisecond * 10)
			return 1, nil
		})

		return nil
	})

	<-fnCalled

	value, err := cache.Get(ctx, "key", func() (interface{}, error) {
		return 0, nil
	})

	require.NoError(t, err)
	require.Equal(t, 1, value)
}

func TestCache_Add_and_GetCached(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := New(Options{Capacity: 2, Expiration: time.Millisecond})

	// Never added.
	_, cached := cache.GetCached(ctx, "key1")
	require.False(t, cached, "GetCached -> cached")

	// Never added before.
	replaced := cache.Add(ctx, "key1", 1)
	require.False(t, replaced, "Add -> replaced")
	value, cached := cache.GetCached(ctx, "key1")
	require.True(t, cached, "GetCached -> cached")
	require.Equal(t, 1, value)
	// Added before.
	replaced = cache.Add(ctx, "key1", 1)
	require.True(t, replaced, "Add -> replaced")

	// Added before but expired.
	time.Sleep(time.Millisecond)
	replaced = cache.Add(ctx, "key1", 1)
	require.False(t, replaced, "Add -> replaced")

	// Never added before.
	replaced = cache.Add(ctx, "key2", 2)
	require.False(t, replaced, "Add -> replaced")
	replaced = cache.Add(ctx, "key3", 3)
	require.False(t, replaced, "Add -> replaced")

	// Evicted because of capacity limit.
	_, cached = cache.GetCached(ctx, "key1")
	require.False(
		t,
		cached,
		"GetCached -> cached (evicted because it was the least recently used)",
	)
	value, cached = cache.GetCached(ctx, "key2")
	require.True(t, cached, "GetCached -> cached")
	require.Equal(t, 2, value)
	value, cached = cache.GetCached(ctx, "key3")
	require.True(t, cached, "GetCached -> cached")
	require.Equal(t, 3, value)
}

func TestCache_Add_and_GetCached_Fuzz(t *testing.T) {
	const numEntries = 200
	require.Zero(t, numEntries%2) // Ensure that numEntries is even.

	ctx := testcontext.New(t)
	cache := NewOf[int64](Options{Capacity: numEntries})

	// Spin up 2 Goroutines that add values and counts the added elements for
	// each one.

	var addCounter1 int64 = -1
	ctx.Go(func() error {
		for e := int64(0); e < numEntries/2; e++ {
			replaced := cache.Add(ctx, strconv.FormatInt(e, 10), e)
			atomic.AddInt64(&addCounter1, 1)

			require.False(t, replaced, "replaced")
		}

		return nil
	})

	var addCounter2 int64 = (numEntries / 2) - 1
	ctx.Go(func() error {
		for e := int64(numEntries / 2); e < numEntries; e++ {
			replaced := cache.Add(ctx, strconv.FormatInt(e, 10), e)
			atomic.AddInt64(&addCounter2, 1)

			require.False(t, replaced, "replaced")
		}

		return nil
	})

	// Spin up 2 Goroutines for getting values, one uses keys that exist and
	// another one that use keys that don't exist.

	ctx.Go(func() error {
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))

		for e := 0; e < numEntries; {
			expVal := rng.Int63n(numEntries)
			addCounter1 := atomic.LoadInt64(&addCounter1)
			addCounter2 := atomic.LoadInt64(&addCounter2)
			if expVal > addCounter2 ||
				(expVal > addCounter1 && expVal < numEntries/2) {
				// The value isn't in the cache yet.
				continue
			}

			e++
			value, cached := cache.GetCached(ctx, strconv.FormatInt(expVal, 10))
			require.True(t, cached, "cached")
			require.Equal(t, expVal, value, "value")
		}

		return nil
	})

	ctx.Go(func() error {
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))

		for e := uint64(0); e < numEntries; e++ {
			key := rng.Int63n(numEntries) + numEntries

			_, cached := cache.GetCached(ctx, strconv.FormatInt(key, 10))
			require.False(t, cached, "cached")
		}

		return nil
	})

	ctx.Wait()
}

//
// helper
//

type checker struct {
	t     *testing.T
	cache *ExpiringLRUOf[string]
	calls int
}

func newChecker(
	t *testing.T,
	cache *ExpiringLRUOf[string],
) func(ctx context.Context, key string, calls int) {
	return (&checker{t: t, cache: cache}).Check
}

func (c *checker) makeCallback(v string) func() (string, error) {
	return func() (string, error) {
		c.calls++
		return v, nil
	}
}

func (c *checker) Check(ctx context.Context, key string, calls int) {
	value, err := c.cache.Get(ctx, key, c.makeCallback(key))
	require.Equal(c.t, c.calls, calls)
	require.Equal(c.t, value, key)
	require.NoError(c.t, err)
}

// Add must keep the order list and the map in step. When it evicted, it
// deleted the map entry and left the list element behind, so the list grew
// without bound and every later eviction picked a key that was already gone -
// which let the map grow past Capacity too.
func TestCache_Add_EvictsFromBothTheMapAndTheList(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[int](Options{Capacity: 2})

	for i := range 100 {
		cache.Add(ctx, strconv.Itoa(i), i)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.LessOrEqual(t, len(cache.data), 2, "the map must respect Capacity")
	require.Equal(t, len(cache.data), cache.order.Len(),
		"an evicted key must leave neither a map entry nor a list element")
}

// Replacing a key must not evict another one: the entry count does not grow.
func TestCache_Add_ReplacingDoesNotEvict(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[int](Options{Capacity: 2})
	cache.Add(ctx, "a", 1)
	cache.Add(ctx, "b", 2)

	require.True(t, cache.Add(ctx, "a", 10), "a was there")

	_, cached := cache.GetCached(ctx, "b")
	require.True(t, cached, "replacing a must not have evicted b")
	value, cached := cache.GetCached(ctx, "a")
	require.True(t, cached)
	require.Equal(t, 10, value)
}

// A non-positive Capacity disables the cache, which Get already honors. Add
// used to read the back of an empty list and panic on it.
func TestCache_Add_WithCachingDisabled(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[int](Options{Capacity: 0})

	require.False(t, cache.Add(ctx, "a", 1))
	_, cached := cache.GetCached(ctx, "a")
	require.False(t, cached, "a disabled cache stores nothing")
}

// An entry that carries its own expiry - a signed URL, a download ticket, an
// access token - must stop being served before that expiry, or a caller is
// handed one that whatever issued it is about to refuse.
func TestCache_AddUntil_StopsServingEarly(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 2, EarlyExpiry: time.Minute})

	now := time.Now()
	expiry := now.Add(10 * time.Minute)
	require.False(t, cache.AddUntil(ctx, "ticket", "signed", expiry))

	value, cached := cache.GetCached(ctx, "ticket")
	require.True(t, cached, "well clear of the expiry")
	require.Equal(t, "signed", value)

	// Nine minutes on it is still outside the one-minute margin.
	at := func(d time.Duration) context.Context {
		timeCtx, _ := time2.WithNewMachine(
			ctx, time2.WithTimeAt(now.Add(d)),
		)
		return timeCtx
	}
	_, cached = cache.GetCached(at(8*time.Minute), "ticket")
	require.True(t, cached, "two minutes of validity left is still served")

	// At the margin it is gone, a whole minute before it actually expires.
	_, cached = cache.GetCached(at(9*time.Minute), "ticket")
	require.False(t, cached,
		"a value about to be refused must not be handed out")
}

// Get reads the same predicate, so an AddUntil entry is not served stale to a
// caller that came through the filling path.
func TestCache_AddUntil_IsHonoredByGet(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 2, EarlyExpiry: time.Minute})

	now := time.Now()
	cache.AddUntil(ctx, "ticket", "stale", now.Add(2*time.Minute))

	timeCtx, _ := time2.WithNewMachine(
		ctx, time2.WithTimeAt(now.Add(90*time.Second)),
	)
	value, err := cache.Get(timeCtx, "ticket", func() (string, error) {
		return "fresh", nil
	})
	require.NoError(t, err)
	require.Equal(t, "fresh", value, "the entry was inside its margin")
}

// A zero EarlyExpiry is no margin at all: the value is served until the
// instant it expires.
func TestCache_AddUntil_WithoutAMargin(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 2})

	now := time.Now()
	cache.AddUntil(ctx, "ticket", "signed", now.Add(time.Minute))

	at := func(d time.Duration) context.Context {
		timeCtx, _ := time2.WithNewMachine(
			ctx, time2.WithTimeAt(now.Add(d)),
		)
		return timeCtx
	}
	_, cached := cache.GetCached(at(59*time.Second), "ticket")
	require.True(t, cached)
	_, cached = cache.GetCached(at(time.Minute), "ticket")
	require.False(t, cached, "expired is expired")
}

// A value that is already inside its margin is worth nothing, so it is not
// stored - and it still invalidates what it replaced, because it is the
// current value and it is dead.
func TestCache_AddUntil_RefusesAnExpiredValue(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 2, EarlyExpiry: time.Minute})
	require.False(t, cache.AddUntil(ctx, "ticket", "old", time.Now().Add(time.Hour)))

	replaced := cache.AddUntil(
		ctx, "ticket", "already dead", time.Now().Add(30*time.Second),
	)
	require.True(t, replaced, "the live entry it displaced is reported")

	_, cached := cache.GetCached(ctx, "ticket")
	require.False(t, cached, "neither value is served")

	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Empty(t, cache.data)
	require.Zero(t, cache.order.Len(), "and nothing is left in either half")
}

// Expiration still governs staleness on top of the entry's own expiry,
// whichever runs out first.
func TestCache_AddUntil_StillObeysExpiration(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 2, Expiration: time.Second})

	now := time.Now()
	cache.AddUntil(ctx, "ticket", "signed", now.Add(time.Hour))

	timeCtx, _ := time2.WithNewMachine(
		ctx, time2.WithTimeAt(now.Add(2*time.Second)),
	)
	_, cached := cache.GetCached(timeCtx, "ticket")
	require.False(t, cached, "the TTL from insertion ran out first")
}

// A key seeded by Add or AddUntil must be served by Get, not refilled. Before,
// the inserted entry carried an unused sync.Once, so the first Get ran its fill
// function and overwrote the value - which for AddUntil also discarded the
// expiry the entry was added with.
func TestCache_GetServesAnAddedValue(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	cache := NewOf[string](Options{Capacity: 4, EarlyExpiry: time.Minute})

	cache.Add(ctx, "added", "by Add")
	value, err := cache.Get(ctx, "added", func() (string, error) {
		return "by Get", nil
	})
	require.NoError(t, err)
	require.Equal(t, "by Add", value, "Get must not refill a seeded key")

	cache.AddUntil(ctx, "ticket", "signed", time.Now().Add(time.Hour))
	value, err = cache.Get(ctx, "ticket", func() (string, error) {
		return "minted again", nil
	})
	require.NoError(t, err)
	require.Equal(t, "signed", value)

	// And once it is inside its margin, Get does fill it.
	timeCtx, _ := time2.WithNewMachine(
		ctx, time2.WithTimeAt(time.Now().Add(time.Hour)),
	)
	value, err = cache.Get(timeCtx, "ticket", func() (string, error) {
		return "minted again", nil
	})
	require.NoError(t, err)
	require.Equal(t, "minted again", value)
}
