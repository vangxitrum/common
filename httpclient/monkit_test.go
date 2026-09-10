package httpclient_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

func TestRequestsAreCountedByMethodAndStatusClass(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusOK),
		status(http.StatusInternalServerError),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	config := testConfig(t)
	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	for range 2 {
		resp, err := get(t, ctx, client, server.URL)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	require.Equal(
		t,
		float64(1),
		seriesValue(t, "http_request", config.Name, "value",
			map[string]string{"method": http.MethodGet, "status_class": "2xx"}),
	)
	require.Equal(
		t,
		float64(1),
		seriesValue(t, "http_request", config.Name, "value",
			map[string]string{"method": http.MethodGet, "status_class": "5xx"}),
	)

	// One sample per wire round trip.
	require.Equal(
		t,
		float64(2),
		seriesValue(t, "http_request_duration", config.Name, "count", nil),
	)
}

// The histogram carries the client name and nothing else. A per-method or
// per-status histogram would put a locked map lookup on every outbound call to
// produce a breakdown the counter already gives.
func TestTheDurationHistogramCarriesOnlyTheClientName(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(status(http.StatusOK))
	defer server.Close()

	config := testConfig(t)
	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	var checked bool
	for _, key := range seriesKeys(t, config.Name) {
		if key.Measurement != "http_request_duration" {
			continue
		}
		checked = true
		// monkit adds the scope itself; name is the only tag this package
		// puts on the histogram, and that is the part under test.
		require.Equal(t, map[string]string{
			"name":  config.Name,
			"scope": "gitlab.internal/tuan.quang.tran/aioz-common/httpclient",
		}, key.Tags.All(), "the request histogram must carry only the client name")
	}
	require.True(
		t,
		checked,
		"the histogram must exist for this to prove anything",
	)
}

// A path carries ids and a query string carries tokens. Neither belongs in a
// series key, and monkit has no cardinality limiter that would catch it.
func TestNoURLReachesASeriesKey(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(status(http.StatusOK))
	defer server.Close()

	config := testConfig(t)
	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client,
		server.URL+"/accounts/42/invoices?token=super-secret")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	keys := seriesKeys(t, config.Name)
	require.NotEmpty(t, keys)

	for _, key := range keys {
		rendered := key.String()
		for _, forbidden := range []string{"super-secret", "/accounts", "invoices", "token"} {
			require.NotContains(t, rendered, forbidden,
				"a url reached a series key: %s", rendered)
		}
		// The host is the other unbounded dimension, and is off by default.
		require.NotContains(
			t,
			rendered,
			strings.TrimPrefix(server.URL, "http://"),
		)
	}
}

// The escape hatch exists, and it is off unless asked for.
func TestHostTagIsOptIn(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(status(http.StatusOK))
	defer server.Close()

	config := testConfig(t)
	config.HostTag = true

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(
		t,
		float64(1),
		seriesValue(
			t,
			"http_request_by_host",
			config.Name,
			"value",
			map[string]string{
				"host":         "127.0.0.1",
				"method":       http.MethodGet,
				"status_class": "2xx",
			},
		),
	)
}

func TestMetricsCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(status(http.StatusOK))
	defer server.Close()

	config := testConfig(t)
	config.Metrics = false

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	// The series is declared either way - monkit registers a value when it is
	// created - so what "off" means is that nothing is recorded into it.
	require.Zero(t, seriesValue(t, "http_request_duration", config.Name, "count", nil))
}
