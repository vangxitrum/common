package debug

import (
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-stats/testcontext"
)

// testLogger returns a slog.Logger that writes to t.Log.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (tw *testWriter) Write(p []byte) (int, error) {
	tw.t.Log(string(p))
	return len(p), nil
}

func TestServer_PrometheusMetrics(t *testing.T) {
	ctx := testcontext.New(t)
	registry := monkit.NewRegistry()
	srv := NewServer(testLogger(t), nil, registry, Config{})

	registry.ScopeNamed("test").Chain(
		monkit.StatSourceFunc(func(cb func(key monkit.SeriesKey, field string, val float64)) {
			cb(monkit.NewSeriesKey("m1"), "f1", 1)
			cb(monkit.NewSeriesKey("m2"), "f2", 2)
			cb(monkit.NewSeriesKey("m1"), "f3", 3)
			cb(monkit.NewSeriesKey("m3"), "", 4)
		}))

	rec := httptest.NewRecorder()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	require.NoError(t, err)
	srv.PrometheusEndpoint.PrometheusMetrics(rec, req)

	const (
		m1 = `# TYPE m1 gauge
m1{scope="test",field="f1"} 1
m1{scope="test",field="f3"} 3
`
		m2 = `# TYPE m2 gauge
m2{scope="test",field="f2"} 2
`
		m3 = `# TYPE m3 gauge
m3{scope="test",field=""} 4
`
	)

	body := rec.Body.String()

	if body != m1+m2+m3 && body != m3+m2+m1 && body != m3+m1+m2 && body != m2+m1+m3 && body != m2+m3+m1 {
		t.Fatalf("string not a combination of m1,m2,m3:\nbody:%q\nm1:%q\nm2:%q\nm3:%q", body, m1, m2, m3)
	}
}

func TestServeTop(t *testing.T) {
	ctx := testcontext.New(t)

	rc := newRequestCounter()

	for range 2 {
		rec := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/one", nil)
		require.NoError(t, err)
		rc.middleware(http.NotFoundHandler()).ServeHTTP(rec, req)
	}

	rec := httptest.NewRecorder()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/top", nil)
	require.NoError(t, err)
	rc.ServeHTTP(rec, req)

	require.Contains(t, rec.Body.String(), "http_requests_count uri=/one 2")
}

func TestServer_Close(t *testing.T) {
	ctx := testcontext.New(t)

	for range 1000 {
		registry := monkit.NewRegistry()
		lis := newFakeListener()
		srv := NewServer(testLogger(t), lis, registry, Config{
			Crawlspace: true,
		})
		go func() { _ = srv.Close() }()
		require.NoError(t, srv.Run(ctx))
	}
}

type fakeListener struct {
	once sync.Once
	ch   chan struct{}
}

func newFakeListener() net.Listener {
	return &fakeListener{
		ch: make(chan struct{}),
	}
}

func (f *fakeListener) Addr() net.Addr { return nil }

func (f *fakeListener) Accept() (net.Conn, error) {
	<-f.ch
	return nil, errors.New("closed")
}

func (f *fakeListener) Close() error {
	f.once.Do(func() { close(f.ch) })
	return nil
}

func TestPrometheusMetrics_GzipCompression(t *testing.T) {
	ctx := testcontext.New(t)
	registry := monkit.NewRegistry()

	t.Run("supported by client", func(t *testing.T) {
		endpoint := NewPrometheusEndpoint(registry)

		registry.ScopeNamed("test").Chain(
			monkit.StatSourceFunc(func(cb func(key monkit.SeriesKey, field string, val float64)) {
				cb(monkit.NewSeriesKey("test_metric"), "field1", 42)
			}))

		rec := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil)
		require.NoError(t, err)
		req.Header.Set("Accept-Encoding", "gzip, deflate")

		endpoint.PrometheusMetrics(rec, req)

		require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))

		gzipReader, err := gzip.NewReader(rec.Body)
		require.NoError(t, err)
		defer ctx.Check(gzipReader.Close)

		decompressed, err := io.ReadAll(gzipReader)
		require.NoError(t, err)

		body := string(decompressed)
		require.Contains(t, body, "# TYPE test_metric gauge")
		require.Contains(t, body, "test_metric{scope=\"test\",field=\"field1\"} 42")
	})

	t.Run("not supported by client", func(t *testing.T) {
		endpoint := NewPrometheusEndpoint(registry)

		registry.ScopeNamed("test").Chain(
			monkit.StatSourceFunc(func(cb func(key monkit.SeriesKey, field string, val float64)) {
				cb(monkit.NewSeriesKey("test_metric"), "field1", 42)
			}))

		for _, encoding := range []string{"", "deflate"} {
			rec := httptest.NewRecorder()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil)
			require.NoError(t, err)
			if encoding != "" {
				req.Header.Set("Accept-Encoding", "deflate")
			}

			endpoint.PrometheusMetrics(rec, req)

			require.Empty(t, rec.Header().Get("Content-Encoding"))
			body := rec.Body.String()
			require.Contains(t, body, "# TYPE test_metric gauge")
			require.Contains(t, body, "test_metric{scope=\"test\",field=\"field1\"} 42")
		}
	})
}
