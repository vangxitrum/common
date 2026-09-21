package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
)

// capture is a fake remote_write receiver: it decodes what the client pushed
// so a test can assert on the series rather than on bytes.
type capture struct {
	*httptest.Server

	t       *testing.T
	status  int
	headers []http.Header
	batches [][]timeSeries
}

func newCapture(t *testing.T) *capture {
	t.Helper()

	c := &capture{t: t, status: http.StatusNoContent}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		raw, err := snappy.Decode(nil, body)
		require.NoError(t, err)

		c.headers = append(c.headers, r.Header.Clone())
		c.batches = append(c.batches, decodeWriteRequest(t, raw))
		w.WriteHeader(c.status)
	}))
	t.Cleanup(c.Close)
	return c
}

// series flattens every batch received so far.
func (c *capture) series() []timeSeries {
	var all []timeSeries
	for _, b := range c.batches {
		all = append(all, b...)
	}
	return all
}

// find returns the first series whose __name__ and field labels match.
func find(series []timeSeries, name, field string) (timeSeries, bool) {
	for _, s := range series {
		var gotName, gotField string
		for _, l := range s.Labels {
			switch l.Name {
			case "__name__":
				gotName = l.Value
			case "field":
				gotField = l.Value
			}
		}
		if gotName == name && (field == "" || gotField == field) {
			return s, true
		}
	}
	return timeSeries{}, false
}

func labelValue(ts timeSeries, name string) string {
	for _, l := range ts.Labels {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(url string) Config {
	return Config{
		URL:           url,
		App:           "testapp",
		Env:           "testenv",
		Instance:      "testinstance",
		Role:          "testrole",
		Interval:      time.Hour,
		DropQuantiles: true,
	}
}

func TestClient_PushesMonkitSeries(t *testing.T) {
	recv := newCapture(t)

	registry := monkit.NewRegistry()
	scope := registry.ScopeNamed("testscope")
	scope.Counter("widgets").Inc(3)

	client, err := NewClient(testLogger(), testConfig(recv.URL), registry)
	require.NoError(t, err)

	require.NoError(t, client.report(context.Background()))

	// A monkit Counter publishes value/high/low, not a single "total".
	got, ok := find(recv.series(), "widgets", "value")
	require.True(t, ok, "widgets counter should have been pushed")
	require.Equal(t, float64(3), got.Samples[0].Value)
	require.Equal(t, "testapp", labelValue(got, "app"))
	require.Equal(t, "testenv", labelValue(got, "env"))
	require.Equal(t, "testinstance", labelValue(got, "instance"))
	require.Equal(t, "testrole", labelValue(got, "role"))
	require.NotZero(t, got.Samples[0].Timestamp)
}

func TestClient_LabelsAreSorted(t *testing.T) {
	recv := newCapture(t)

	registry := monkit.NewRegistry()
	registry.ScopeNamed("s").Counter("c").Inc(1)

	client, err := NewClient(testLogger(), testConfig(recv.URL), registry)
	require.NoError(t, err)
	require.NoError(t, client.report(context.Background()))

	// Receivers reject an unsorted label set, and an unstable order breaks
	// series identity across pushes.
	for _, s := range recv.series() {
		require.True(t, len(s.Labels) > 0)
		for i := 1; i < len(s.Labels); i++ {
			require.Less(t, s.Labels[i-1].Name, s.Labels[i].Name,
				"labels must be sorted by name: %v", s.Labels)
		}
	}
}

func TestClient_EmptyLabelsAreDropped(t *testing.T) {
	recv := newCapture(t)

	registry := monkit.NewRegistry()
	registry.ScopeNamed("s").Counter("c").Inc(1)

	cfg := testConfig(recv.URL)
	cfg.Role = "" // a service that does not set a role
	cfg.Env = ""  // ... nor an environment

	client, err := NewClient(testLogger(), cfg, registry)
	require.NoError(t, err)
	require.NoError(t, client.report(context.Background()))

	for _, s := range recv.series() {
		for _, l := range s.Labels {
			require.NotEqual(t, "role", l.Name, "an empty label should not be sent")
			require.NotEqual(t, "env", l.Name, "an empty label should not be sent")
		}
	}
}

func TestClient_CardinalityFilters(t *testing.T) {
	t.Run("include prefix", func(t *testing.T) {
		recv := newCapture(t)
		registry := monkit.NewRegistry()
		registry.ScopeNamed("s").Counter("keep_me").Inc(1)
		registry.ScopeNamed("s").Counter("drop_me").Inc(1)

		cfg := testConfig(recv.URL)
		cfg.IncludePrefixes = []string{"keep_"}

		client, err := NewClient(testLogger(), cfg, registry)
		require.NoError(t, err)
		require.NoError(t, client.report(context.Background()))

		_, ok := find(recv.series(), "keep_me", "")
		require.True(t, ok)
		_, ok = find(recv.series(), "drop_me", "")
		require.False(t, ok)
	})

	t.Run("exclude prefix", func(t *testing.T) {
		recv := newCapture(t)
		registry := monkit.NewRegistry()
		registry.ScopeNamed("s").Counter("noisy_thing").Inc(1)
		registry.ScopeNamed("s").Counter("quiet_thing").Inc(1)

		cfg := testConfig(recv.URL)
		cfg.ExcludePrefixes = []string{"noisy_"}

		client, err := NewClient(testLogger(), cfg, registry)
		require.NoError(t, err)
		require.NoError(t, client.report(context.Background()))

		_, ok := find(recv.series(), "noisy_thing", "")
		require.False(t, ok)
		_, ok = find(recv.series(), "quiet_thing", "")
		require.True(t, ok)
	})

	t.Run("drop quantiles", func(t *testing.T) {
		recv := newCapture(t)
		registry := monkit.NewRegistry()
		registry.ScopeNamed("s").DurationVal("latency").Observe(time.Second)

		client, err := NewClient(testLogger(), testConfig(recv.URL), registry)
		require.NoError(t, err)
		require.NoError(t, client.report(context.Background()))

		// The reservoir fields are the bulk of a mon.Task's cardinality.
		for _, dropped := range []string{"r10", "r50", "r90", "r99", "rmin", "rmax"} {
			_, ok := find(recv.series(), "latency", dropped)
			require.False(t, ok, "%s should have been dropped", dropped)
		}
		// while the exact aggregates survive.
		_, ok := find(recv.series(), "latency", "count")
		require.True(t, ok)
	})
}

func TestClient_Auth(t *testing.T) {
	t.Run("bearer wins over basic", func(t *testing.T) {
		recv := newCapture(t)
		registry := monkit.NewRegistry()
		registry.ScopeNamed("s").Counter("c").Inc(1)

		cfg := testConfig(recv.URL)
		cfg.BearerToken = "tok"
		cfg.Username, cfg.Password = "u", "p"

		client, err := NewClient(testLogger(), cfg, registry)
		require.NoError(t, err)
		require.NoError(t, client.report(context.Background()))

		require.Equal(t, "Bearer tok", recv.headers[0].Get("Authorization"))
	})

	t.Run("basic auth", func(t *testing.T) {
		recv := newCapture(t)
		registry := monkit.NewRegistry()
		registry.ScopeNamed("s").Counter("c").Inc(1)

		cfg := testConfig(recv.URL)
		cfg.Username, cfg.Password = "u", "p"

		client, err := NewClient(testLogger(), cfg, registry)
		require.NoError(t, err)
		require.NoError(t, client.report(context.Background()))

		require.Equal(t, "Basic dTpw", recv.headers[0].Get("Authorization"))
	})
}

func TestClient_RemoteWriteHeaders(t *testing.T) {
	recv := newCapture(t)
	registry := monkit.NewRegistry()
	registry.ScopeNamed("s").Counter("c").Inc(1)

	client, err := NewClient(testLogger(), testConfig(recv.URL), registry)
	require.NoError(t, err)
	require.NoError(t, client.report(context.Background()))

	h := recv.headers[0]
	require.Equal(t, "snappy", h.Get("Content-Encoding"))
	require.Equal(t, "application/x-protobuf", h.Get("Content-Type"))
	require.Equal(t, "0.1.0", h.Get("X-Prometheus-Remote-Write-Version"))
}

func TestClient_RejectedPushIsAnError(t *testing.T) {
	recv := newCapture(t)
	recv.status = http.StatusBadRequest

	registry := monkit.NewRegistry()
	registry.ScopeNamed("s").Counter("c").Inc(1)

	client, err := NewClient(testLogger(), testConfig(recv.URL), registry)
	require.NoError(t, err)

	err = client.report(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "status=400")
}

func TestClient_RunIsANoopWhenDisabled(t *testing.T) {
	recv := newCapture(t)

	cfg := testConfig(recv.URL)
	cfg.URL = "" // empty URL disables the client

	client, err := NewClient(testLogger(), cfg, monkit.NewRegistry())
	require.NoError(t, err)

	require.NoError(t, client.Run(context.Background()))
	require.Empty(t, recv.batches, "a disabled client must not push")
}

func TestClient_BatchesLargeRegistries(t *testing.T) {
	recv := newCapture(t)

	registry := monkit.NewRegistry()
	scope := registry.ScopeNamed("s")
	for i := range batchSeries + 10 {
		scope.Counter(fmt.Sprintf("counter_%d", i)).Inc(1)
	}

	client, err := NewClient(testLogger(), testConfig(recv.URL), registry)
	require.NoError(t, err)
	require.NoError(t, client.report(context.Background()))

	require.Greater(t, len(recv.batches), 1, "should have been split into batches")
	for _, b := range recv.batches {
		require.LessOrEqual(t, len(b), batchSeries)
	}
}

func TestClient_GathererSeries(t *testing.T) {
	recv := newCapture(t)

	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total"},
		[]string{"method"},
	)
	reg.MustRegister(counter)
	counter.WithLabelValues("GET").Add(7)

	histogram := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Buckets: []float64{0.1, 1},
	})
	reg.MustRegister(histogram)
	histogram.Observe(0.05)

	client, err := NewClient(testLogger(), testConfig(recv.URL), monkit.NewRegistry(), WithGatherer(reg))
	require.NoError(t, err)
	require.NoError(t, client.report(context.Background()))

	got, ok := find(recv.series(), "http_requests_total", "")
	require.True(t, ok)
	require.Equal(t, float64(7), got.Samples[0].Value)
	require.Equal(t, "GET", labelValue(got, "method"))
	require.Equal(t, "testapp", labelValue(got, "app"), "push labels apply to gathered metrics too")
	require.Equal(t, "testenv", labelValue(got, "env"), "push labels apply to gathered metrics too")

	sum, ok := find(recv.series(), "http_request_duration_seconds_sum", "")
	require.True(t, ok)
	require.InDelta(t, 0.05, sum.Samples[0].Value, 1e-9)

	count, ok := find(recv.series(), "http_request_duration_seconds_count", "")
	require.True(t, ok)
	require.Equal(t, float64(1), count.Samples[0].Value)

	// The +Inf bucket is implicit in the protobuf form but required for
	// histogram_quantile to have an upper bound.
	var sawInf bool
	for _, s := range recv.series() {
		if labelValue(s, "__name__") == "http_request_duration_seconds_bucket" &&
			labelValue(s, "le") == "+Inf" {
			sawInf = true
			require.Equal(t, float64(1), s.Samples[0].Value)
		}
	}
	require.True(t, sawInf, "a +Inf bucket must be emitted")
}

func TestClient_UsesAHealthCheckedTransport(t *testing.T) {
	client, err := NewClient(testLogger(), testConfig("http://example.invalid"), monkit.NewRegistry())
	require.NoError(t, err)

	transport, ok := client.http.Transport.(*http.Transport)
	require.True(t, ok, "the push client should carry its own transport")

	// Health checks are what keep a dead pooled connection from stalling every
	// push until the deadline.
	require.NotNil(t, transport.HTTP2,
		"http/2 health checks should already be configured")
	require.Equal(t,
		httpclient.DefaultH2HealthCheckInterval, transport.HTTP2.SendPingTimeout)
	require.Equal(t,
		httpclient.DefaultH2PingTimeout, transport.HTTP2.PingTimeout)
}

func TestParseContainerID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"cgroup v1":       {"12:cpu:/docker/" + id, "0123456789ab"},
		"cgroup v2 scope": {"0::/system.slice/docker-" + id + ".scope", "0123456789ab"},
		"cri-containerd":  {"0::/kubepods/cri-containerd-" + id + ".scope", "0123456789ab"},
		"not a container": {"0::/user.slice/user-1000.slice", ""},
		"too short":       {"0::/docker/abc", ""},
		"not hex":         {"0::/docker/" + "z" + id[1:], ""},
		"empty":           {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, parseContainerID(tc.in))
		})
	}
}

func TestSanitize(t *testing.T) {
	require.Equal(t, "a_b_c", sanitize("a.b-c"))
	require.Equal(t, "_1abc", sanitize("1abc"), "a leading digit is prefixed")
	require.Equal(t, "", sanitize(""))
}

func TestFormatFloat(t *testing.T) {
	require.Equal(t, "0.1", formatFloat(0.1))
	require.Equal(t, "1", formatFloat(1))
	require.Equal(t, "0.5", formatFloat(0.5))
}
