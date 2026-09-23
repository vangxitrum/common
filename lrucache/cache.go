package lrucache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/time2"

	"github.com/spacemonkeygo/monkit/v3"
)

var mon = monkit.Package()

// Options controls the details of the expiration policy.
type Options struct {
	// Expiration is how long an entry will be valid. It is not
	// affected by LRU or anything: after this duration, the object
	// is invalidated. A non-positive value means no expiration.
	Expiration time.Duration

	// EarlyExpiry is how long before its own expiry an entry added with
	// AddUntil stops being served, so a caller never receives a value that is
	// about to be rejected by whatever issued it - a download ticket, a
	// presigned URL, an access token. The value is handed out only while
	// now+EarlyExpiry is still before its expiry.
	//
	// It applies to AddUntil entries only. Expiration is measured from
	// insertion, so subtracting a margin from it would just be a shorter
	// Expiration.
	EarlyExpiry time.Duration

	// Capacity is how many objects to keep in memory.
	Capacity int

	// Name is used to differentiate cache in monkit stat.
	Name string
}

// cacheState contains all of the state for a cached entry.
type cacheState[T any] struct {
	once sync.Once
	when time.Time

	// expiresAt is the entry's own absolute expiry, from AddUntil. The zero
	// time means it has none and only Options.Expiration applies.
	expiresAt time.Time

	order  *list.Element
	value  T
	loaded bool
}

// ExpiringLRU is a backwards compatible implementation of ExpiringLRU.
type ExpiringLRU = ExpiringLRUOf[any]

// ExpiringLRUOf caches values for string keys with a time based expiration and
// an LRU based eviciton policy.
type ExpiringLRUOf[T any] struct {
	mu    sync.Mutex
	opts  Options
	data  map[string]*cacheState[T]
	order *list.List
}

// New constructs an ExpiringLRU with the given options.
func New(opts Options) *ExpiringLRU {
	return NewOf[any](opts)
}

// NewOf constructs an ExpiringLRU with the given options.
func NewOf[T any](opts Options) *ExpiringLRUOf[T] {
	return &ExpiringLRUOf[T]{
		opts:  opts,
		data:  make(map[string]*cacheState[T], opts.Capacity),
		order: list.New(),
	}
}

// Reset resets the lru cache.
func (e *ExpiringLRUOf[T]) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.order = list.New()
	for key := range e.data {
		delete(e.data, key)
	}
}

// Get returns the value for some key if it exists and is valid. If not
// it will call the provided function. Concurrent calls will dedupe as
// best as they are able. If the function returns an error, it is not
// cached and further calls will try again.
func (e *ExpiringLRUOf[T]) Get(
	ctx context.Context,
	key string,
	fn func() (T, error),
) (value T, err error) {
	if e.opts.Capacity <= 0 {
		e.monitorCache(false)
		return fn()
	}

	for {
		e.mu.Lock()

		state, ok := e.data[key]
		switch {
		case !ok:
			for len(e.data) >= e.opts.Capacity {
				back := e.order.Back()
				delete(e.data, back.Value.(string))
				e.order.Remove(back)
			}
			state = &cacheState[T]{
				when:  time2.Now(ctx),
				order: e.order.PushFront(key),
			}
			e.data[key] = state

		case e.expired(ctx, state):
			delete(e.data, key)
			e.order.Remove(state.order)
			e.mu.Unlock()
			continue

		default:
			e.order.MoveToFront(state.order)
		}

		e.mu.Unlock()

		called := false
		state.once.Do(func() {
			called = true
			value, err = fn()

			if err == nil {
				// careful because we don't want a `(*T)(nil) != nil` situation
				// that's why we only assign to state.value if err == nil.
				state.value = value
				state.loaded = true
			} else {
				// the once has been used. delete it so that any other waiters
				// will retry.
				e.mu.Lock()
				if e.data[key] == state {
					delete(e.data, key)
					e.order.Remove(state.order)
				}
				e.mu.Unlock()
			}
		})

		if called || state.loaded {
			e.monitorCache(!called)
			return state.value, err
		}
	}
}

func (e *ExpiringLRUOf[T]) monitorCache(valueFromCache bool) {
	if e.opts.Name == "" {
		return
	}

	nameTag := monkit.NewSeriesTag("name", e.opts.Name)
	if valueFromCache {
		mon.Event("cache_hit", nameTag)
	} else {
		mon.Event("cache_miss", nameTag)
	}
}

// Delete explicitly removes a key from the cache if it exists.
func (e *ExpiringLRUOf[T]) Delete(ctx context.Context, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, ok := e.data[key]
	if !ok {
		return
	}
	delete(e.data, key)
	e.order.Remove(state.order)
}

// Add adds a value to the cache. It expires Options.Expiration after this
// call, like any other entry.
//
// replaced is true if the key already existed in the cache and was valid, hence
// the value is replaced.
func (e *ExpiringLRUOf[T]) Add(
	ctx context.Context,
	key string,
	value T,
) (replaced bool) {
	return e.add(ctx, key, value, time.Time{})
}

// AddUntil adds a value that carries its own expiry, such as a signed URL or
// an access token. The entry stops being served Options.EarlyExpiry before
// expiresAt, so a caller is never handed one that is about to be refused, and
// Options.Expiration still applies on top of it.
//
// An expiresAt already inside the margin is not stored: there would be nothing
// left to serve. Any entry it displaced is still gone, because the new value
// is the current one and it is dead. replaced reports that displacement.
func (e *ExpiringLRUOf[T]) AddUntil(
	ctx context.Context,
	key string,
	value T,
	expiresAt time.Time,
) (replaced bool) {
	return e.add(ctx, key, value, expiresAt)
}

func (e *ExpiringLRUOf[T]) add(
	ctx context.Context,
	key string,
	value T,
	expiresAt time.Time,
) (replaced bool) {
	// A non-positive Capacity disables the cache, the way it does for Get.
	// Before, this read the back of an empty list and panicked on it.
	if e.opts.Capacity <= 0 {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	state, _ := e.peek(ctx, key)
	replaced = state != nil
	if replaced {
		// Taken out of both halves before the eviction loop counts, so
		// replacing a key never evicts a different one to make room for it.
		e.order.Remove(state.order)
		delete(e.data, key)
	}

	state = &cacheState[T]{
		when:      time2.Now(ctx),
		expiresAt: expiresAt,
		value:     value,
		loaded:    true,
	}
	// The fill is spent: a later Get must serve this value rather than run its
	// own function and overwrite it. Get waits on the once whatever loaded
	// says, so the once has to be used up here, not just the flag set.
	state.once.Do(func() {})

	if e.expired(ctx, state) {
		return replaced
	}

	// Evict until there is room for one more. The map and the list are always
	// left in step: dropping the map entry alone, as this did, left the list
	// element behind, so the list grew without bound and every later eviction
	// picked a key that was already gone - which let the map grow past
	// Capacity as well.
	for len(e.data) >= e.opts.Capacity {
		back := e.order.Back()
		if back == nil {
			break
		}
		delete(e.data, back.Value.(string))
		e.order.Remove(back)
	}

	state.order = e.order.PushFront(key)
	e.data[key] = state

	return replaced
}

// GetCached returns the value associated with key and true if it exists and
// hasn't expired, otherwise nil and false.
func (e *ExpiringLRUOf[T]) GetCached(
	ctx context.Context,
	key string,
) (value T, cached bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, _ := e.peek(ctx, key)
	if state == nil {
		var zero T
		return zero, false
	}

	e.order.MoveToFront(state.order)
	return state.value, true
}

// peek returns the state associated to the key if exists and it's valid,
// otherwise nil. evicted is true when the key existed but the state has
// expired.
//
// peek doesn't update the key as being recently used.
//
// NOTE the caller must always lock and unlock the mutex before calling this
// method.
func (e *ExpiringLRUOf[T]) peek(
	ctx context.Context,
	key string,
) (state *cacheState[T], evicted bool) {
	state, ok := e.data[key]
	if !ok {
		return nil, false
	}

	if e.expired(ctx, state) {
		e.order.Remove(state.order)
		delete(e.data, key)

		return nil, true
	}

	return state, false
}

// expired reports whether an entry may no longer be served: its TTL from
// insertion has run out, or its own expiry is closer than Options.EarlyExpiry.
//
// Both Get and peek ask this, so an AddUntil entry is checked the same way
// however it is read.
func (e *ExpiringLRUOf[T]) expired(
	ctx context.Context,
	state *cacheState[T],
) bool {
	if e.opts.Expiration > 0 &&
		time2.Since(ctx, state.when) > e.opts.Expiration {
		return true
	}
	if state.expiresAt.IsZero() {
		return false
	}
	return !time2.Now(ctx).Add(e.opts.EarlyExpiry).Before(state.expiresAt)
}
