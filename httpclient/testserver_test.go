package httpclient_test

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
)

// testLogger routes the package's log lines into the test output, so a failure
// shows what the client was doing.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(
		slog.NewTextHandler(
			&testWriter{t: t},
			&slog.HandlerOptions{Level: slog.LevelDebug},
		),
	)
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p))
	return len(p), nil
}

// testConfig is what config.Bind would produce from the struct tags, which is
// what an operator actually gets.
func testConfig(t *testing.T) httpclient.Config {
	t.Helper()
	return httpclient.Config{
		Name:                  t.Name(),
		Timeout:               30 * time.Second,
		DialTimeout:           30 * time.Second,
		KeepAlive:             30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		ForceHTTP2:            true,
		TLSMinVersion:         "1.2",
		MaxRedirects:          10,
		MaxResponseSize:       32 * memory.MiB,
		RetryMax:              4,
		RetryWaitMin:          time.Second,
		RetryWaitMax:          30 * time.Second,
		Metrics:               true,
	}
}

// script serves one response per call, in order, repeating the last one.
type script struct {
	mu       sync.Mutex
	steps    []http.HandlerFunc
	requests []*recorded
}

type recorded struct {
	method string
	header http.Header
	body   string
}

func newScript(steps ...http.HandlerFunc) *script {
	return &script{steps: steps}
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	index := len(s.requests)
	body := readAll(r)
	s.requests = append(s.requests, &recorded{
		method: r.Method,
		header: r.Header.Clone(),
		body:   body,
	})
	if index >= len(s.steps) {
		index = len(s.steps) - 1
	}
	step := s.steps[index]
	s.mu.Unlock()

	step(w, r)
}

func (s *script) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *script) call(i int) *recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[i]
}

func readAll(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf := make([]byte, 4096)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func statusWithHeader(code int, key, value string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(key, value)
		w.WriteHeader(code)
	}
}

// connCounter counts accepted connections, which is how the tests tell a reused
// pooled connection from a fresh one.
type connCounter struct {
	mu    sync.Mutex
	total int
}

func (c *connCounter) track(server *httptest.Server) {
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			c.mu.Lock()
			c.total++
			c.mu.Unlock()
		}
	}
}

func (c *connCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// seriesValue finds one field of a series this client publishes. Every test
// uses its own client name, which is what makes these safe under t.Parallel.
func seriesValue(
	t *testing.T,
	measurement, name, field string,
	tags map[string]string,
) float64 {
	t.Helper()

	var found float64
	var ok bool
	monkit.Default.Stats(func(key monkit.SeriesKey, f string, val float64) {
		if key.Measurement != measurement || key.Tags.Get("name") != name ||
			f != field {
			return
		}
		for k, v := range tags {
			if key.Tags.Get(k) != v {
				return
			}
		}
		found, ok = val, true
	})

	require.True(
		t,
		ok,
		"no %s series tagged name=%s %v",
		measurement,
		name,
		tags,
	)
	return found
}

// seriesKeys collects every series key this client published, for the negative
// assertion that no URL ever reaches one.
func seriesKeys(t *testing.T, name string) []monkit.SeriesKey {
	t.Helper()

	var keys []monkit.SeriesKey
	seen := map[string]bool{}
	monkit.Default.Stats(func(key monkit.SeriesKey, _ string, _ float64) {
		if key.Tags.Get("name") != name {
			return
		}
		if id := key.String(); !seen[id] {
			seen[id] = true
			keys = append(keys, key)
		}
	})
	return keys
}
