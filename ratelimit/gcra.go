package ratelimit

import (
	"container/list"
	"sync"
)

// The generic cell rate algorithm keeps one number per window: the
// theoretical arrival time (TAT), when the window would be empty again if no
// more requests came. A request moves it one interval later; it is allowed if
// that leaves the TAT no more than Count intervals ahead of now. That is a
// burst of Count and then one request per interval, with an exact wait for
// the next one - and one integer of state, the same in memory and in Redis.
//
// Times are integer microseconds since the Unix epoch. Redis's Lua has only
// doubles, which hold these exactly until the year 2255.

// window is one rate of one key, ready for a store.
type window struct {
	key      string // the store key, prefix included
	rule     string // for metrics
	interval int64  // microseconds between requests at the sustained rate
	limit    int64  // interval * count: how far ahead of now the TAT may run
}

// step is one GCRA check. next is the TAT the window has if the request is
// allowed; wait is zero if it is, or how long until it would be.
func step(tat, now, interval, limit int64) (next, wait int64) {
	tat = max(tat, now)
	next = tat + interval
	if allowAt := next - limit; allowAt > now {
		wait = allowAt - now
	}
	return next, wait
}

// decision is a store's answer: denied is the index of the window with the
// longest wait, or -1 when every window allowed.
type decision struct {
	wait   int64
	denied int
}

var allowed = decision{denied: -1}

// memoryStore is a bounded LRU of TATs. Every window of every key in one call
// is checked and committed under one lock, which is what lrucache cannot do:
// its reads and writes lock separately.
type memoryStore struct {
	capacity int

	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // of *memoryEntry, most recently used first
}

type memoryEntry struct {
	key string
	tat int64
}

func newMemoryStore(capacity int) *memoryStore {
	return &memoryStore{
		capacity: capacity,
		entries:  make(map[string]*list.Element),
		order:    list.New(),
	}
}

func (m *memoryStore) allow(now int64, windows []window) decision {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := make([]int64, len(windows))
	d := allowed
	for i, w := range windows {
		var tat int64
		if el, ok := m.entries[w.key]; ok {
			tat = el.Value.(*memoryEntry).tat
			m.order.MoveToFront(el)
		}
		var wait int64
		next[i], wait = step(tat, now, w.interval, w.limit)
		if wait > d.wait {
			d = decision{wait: wait, denied: i}
		}
	}
	if d.denied >= 0 {
		// Denied requests consume nothing, so hammering a key cannot push its
		// refill further out.
		return d
	}

	for i, w := range windows {
		if el, ok := m.entries[w.key]; ok {
			el.Value.(*memoryEntry).tat = next[i]
			continue
		}
		m.entries[w.key] = m.order.PushFront(
			&memoryEntry{key: w.key, tat: next[i]},
		)
	}
	for m.order.Len() > m.capacity {
		oldest := m.order.Back()
		m.order.Remove(oldest)
		delete(m.entries, oldest.Value.(*memoryEntry).key)
		mon.Counter("ratelimit_evicted").Inc(1)
	}
	return d
}

func (m *memoryStore) reset(keys []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, key := range keys {
		if el, ok := m.entries[key]; ok {
			m.order.Remove(el)
			delete(m.entries, key)
		}
	}
}
