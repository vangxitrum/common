# lrucache

An LRU cache with a TTL, where a miss is filled once no matter how many callers
missed at the same time.

The reason to reach for this rather than a map plus a mutex is the third
property. When a hot key expires under load, every in-flight request misses at
once and every one of them calls the backend. That is a thundering herd, and it
arrives exactly when the system is busiest. `Get` takes a function to compute
the value and runs it once per key, with the other callers waiting on the same
result.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/lrucache"
```

## Usage

```go
cache := lrucache.NewOf[*Worker](lrucache.Options{
    Expiration: 5 * time.Minute,
    Capacity:   10000,
})

worker, err := cache.Get(ctx, id, func() (*Worker, error) {
    return db.WorkerByID(ctx, id)   // called once per key, however many miss
})
```

`GetCached` reads without filling, for a path that would rather serve nothing
than wait for the backend. `Add` inserts directly, `Delete` invalidates a key,
`Reset` empties the cache.

## Two things worth knowing

**A failed fill is not cached.** If the function returns an error, the error
goes to the waiting callers and nothing is stored, so the next request tries
again rather than serving a cached failure for the whole TTL.

**Expiration is checked on read, not swept.** An entry past its TTL stays
resident until something asks for it or the LRU evicts it, so `Capacity`
governs memory and `Expiration` governs staleness. They are separate limits and
both need a value. A non-positive `Capacity` disables the cache entirely and
every `Get` calls straight through, which is a useful switch and a silent
mistake if it was not meant.

The clock comes from [`time2`](../time2/README.md), so a test can expire an
entry by advancing a time machine instead of sleeping.

## API

```go
type Options struct {
    Expiration time.Duration // non-positive: entries never expire
    Capacity   int           // non-positive: caching is disabled
    Name       string        // distinguishes this cache in monkit stats
}

func NewOf[T any](opts Options) *ExpiringLRUOf[T]
func New(opts Options) *ExpiringLRU   // = ExpiringLRUOf[any]

func (e *ExpiringLRUOf[T]) Get(ctx context.Context, key string, fn func() (T, error)) (T, error)
func (e *ExpiringLRUOf[T]) GetCached(ctx context.Context, key string) (value T, cached bool)
func (e *ExpiringLRUOf[T]) Add(ctx context.Context, key string, value T) (replaced bool)
func (e *ExpiringLRUOf[T]) Delete(ctx context.Context, key string)
func (e *ExpiringLRUOf[T]) Reset()
```
