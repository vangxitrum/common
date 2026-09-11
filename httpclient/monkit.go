package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
)

// The two bounded dimensions of the request counter. Both sets are closed, and
// both are known at construction, so every counter is created once and the hot
// path is two array indexes rather than a locked map lookup.
var countedMethods = [...]string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodDelete, http.MethodHead, http.MethodOptions, "other",
}

var countedClasses = [...]string{"1xx", "2xx", "3xx", "4xx", "5xx", "error"}

const classError = 5

// metrics records one sample per wire round trip for one client.
type metrics struct {
	name monkit.SeriesTag

	// Created once per client rather than per request: Scope.DurationVal takes
	// a lock and a map lookup, and this is on the path of every outbound call.
	duration *monkit.DurationVal
	counters [len(countedMethods)][len(countedClasses)]*monkit.Counter

	// hostTag is the escape hatch for a client with a small, fixed set of
	// hosts. The set is not known at construction, so unlike the counters above
	// it costs a lock - which is why it is off by default.
	hostTag bool
	mu      sync.RWMutex
	byHost  map[hostKey]*monkit.Counter
	capped  *monkit.Counter
}

type hostKey struct{ host, method, class string }

func newMetrics(name monkit.SeriesTag, hostTag bool) *metrics {
	m := &metrics{
		name:     name,
		duration: mon.DurationVal("http_request_duration", name),
		hostTag:  hostTag,
		byHost:   map[hostKey]*monkit.Counter{},
		capped:   mon.Counter("http_body_capped", name),
	}

	for i, method := range countedMethods {
		for j, class := range countedClasses {
			m.counters[i][j] = mon.Counter("http_request", name,
				monkit.NewSeriesTag("method", method),
				monkit.NewSeriesTag("status_class", class))
		}
	}
	return m
}

// transport times one round trip.
//
// A sample is one attempt on the wire - the dependency's real latency - because
// a retry is a new call. The http_retries counter carries the retries.
func (m *metrics) transport(next http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		// Measurement is not waiting, so this is the real clock rather than
		// time2: a test that advanced a fake clock would be asserting its own
		// arithmetic.
		start := time.Now()
		resp, err := next.RoundTrip(req)
		m.duration.Observe(time.Since(start))

		method := methodIndex(req.Method)
		class := classError
		if err == nil {
			class = classIndex(resp.StatusCode)
		}
		m.counters[method][class].Inc(1)

		if m.hostTag {
			m.hostCounter(req.URL.Hostname(), countedMethods[method], countedClasses[class]).
				Inc(1)
		}

		if err != nil {
			// The failure event is rare, so it can afford the extra tag the
			// hot-path histogram cannot. The kind vocabulary is closed.
			mon.Event("http_request_failed", m.name,
				monkit.NewSeriesTag("kind", errorKind(err)))
		}
		return resp, err
	})
}

func (m *metrics) hostCounter(host, method, class string) *monkit.Counter {
	key := hostKey{host: host, method: method, class: class}

	m.mu.RLock()
	counter, ok := m.byHost[key]
	m.mu.RUnlock()
	if ok {
		return counter
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if counter, ok = m.byHost[key]; ok {
		return counter
	}

	counter = mon.Counter("http_request_by_host", m.name,
		monkit.NewSeriesTag("host", host),
		monkit.NewSeriesTag("method", method),
		monkit.NewSeriesTag("status_class", class))
	m.byHost[key] = counter
	return counter
}

func methodIndex(method string) int {
	for i, known := range countedMethods {
		if known == method {
			return i
		}
	}
	return len(countedMethods) - 1 // "other"
}

func classIndex(status int) int {
	switch {
	case status < 200:
		return 0
	case status < 300:
		return 1
	case status < 400:
		return 2
	case status < 500:
		return 3
	default:
		return 4
	}
}

// errorKind reduces a transport failure to one of a handful of labels.
//
// The vocabulary is closed on purpose. A tag taken from an error string is
// unbounded - it carries addresses, ports and host names - and there is nothing
// in a metrics pipeline that will stop it.
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}

	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuthority) {
		return "tls"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "dial"
	}
	return "other"
}
