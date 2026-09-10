package httpclient_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

// An over-long body must say so. Truncating it silently - which is what
// io.LimitReader would do - turns a cap into "unexpected end of JSON input"
// with nothing to point at the cause.
func TestBodyCapReportsRatherThanTruncating(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	const limit = 1024

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), limit+1))
		}),
	)
	defer server.Close()

	config := testConfig(t)
	config.MaxResponseSize = limit

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	_, err = io.ReadAll(resp.Body)
	require.Error(t, err)
	require.NotErrorIs(
		t,
		err,
		io.EOF,
		"a cap must not present as the end of the body",
	)
	require.Contains(t, err.Error(), "response body exceeded")

	require.Equal(
		t,
		float64(1),
		seriesValue(t, "http_body_capped", config.Name, "value", nil),
	)
}

func TestBodyExactlyAtTheCapIsFine(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	const limit = 1024

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), limit))
		}),
	)
	defer server.Close()

	config := testConfig(t)
	config.MaxResponseSize = limit

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Len(t, body, limit)
}

func TestZeroCapDisablesTheLimit(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	const size = 64 * 1024

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), size))
		}),
	)
	defer server.Close()

	config := testConfig(t)
	config.MaxResponseSize = 0

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Len(t, body, size)
}
