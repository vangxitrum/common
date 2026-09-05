package debug

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

// requestCounter tracks HTTP request counts per URI path.
type requestCounter struct {
	mu     sync.Mutex
	counts map[string]*atomic.Int64
}

func newRequestCounter() *requestCounter {
	return &requestCounter{counts: make(map[string]*atomic.Int64)}
}

func (rc *requestCounter) inc(path string) {
	rc.mu.Lock()
	c, ok := rc.counts[path]
	if !ok {
		c = new(atomic.Int64)
		rc.counts[path] = c
	}
	rc.mu.Unlock()
	c.Add(1)
}

// middleware wraps next and counts every request by URI path.
func (rc *requestCounter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.inc(r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP writes request counts sorted by frequency (highest first).
// Format: "http_requests_count uri=<path> <count>"
func (rc *requestCounter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		path  string
		count int64
	}

	rc.mu.Lock()
	entries := make([]entry, 0, len(rc.counts))
	for path, c := range rc.counts {
		entries = append(entries, entry{path, c.Load()})
	}
	rc.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].path < entries[j].path
	})

	for _, e := range entries {
		_, _ = fmt.Fprintf(
			w,
			"http_requests_count uri=%s %d\n",
			e.path,
			e.count,
		)
	}
}
