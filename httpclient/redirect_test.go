package httpclient_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

func TestRedirectCapIsEnforced(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	hits := 0
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			http.Redirect(w, r, "/again", http.StatusFound)
		}),
	)
	defer server.Close()

	config := testConfig(t)
	config.MaxRedirects = 3

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err)
	require.Contains(t, err.Error(), "stopped after 3 redirects")
	require.Equal(t, 4, hits, "the original request plus its three redirects")
}

// Zero does not mean "fail on any redirect": a caller that wants the Location
// header wants the response, and there would otherwise be no way to say "do not
// follow".
func TestZeroRedirectsReturnsTheResponse(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}),
	)
	defer server.Close()

	config := testConfig(t)
	config.MaxRedirects = 0

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Equal(t, "/elsewhere", resp.Header.Get("Location"))
}

// The one thing Go does not refuse on its own, and the reason this policy
// exists at all.
func TestHTTPSToHTTPDowngradeIsRefused(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	plainHits := 0
	plain := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			plainHits++
			w.WriteHeader(http.StatusOK)
		}),
	)
	defer plain.Close()

	secure := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL, http.StatusFound)
		}),
	)
	defer secure.Close()

	config := testConfig(t)
	config.TLSSkipVerify = true // httptest's certificate is self-signed

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, secure.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err)
	require.Contains(t, err.Error(), "https redirect to http")
	require.Zero(t, plainHits, "nothing may reach the plaintext endpoint")
}

func TestDowngradeCanBeAllowed(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	plain := httptest.NewServer(status(http.StatusOK))
	defer plain.Close()

	secure := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL, http.StatusFound)
		}),
	)
	defer secure.Close()

	config := testConfig(t)
	config.TLSSkipVerify = true
	config.AllowDowngrade = true

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, secure.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// The downgrade rule must not become a rule against redirects generally.
func TestSameSchemeRedirectIsStillFollowed(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	target := httptest.NewServer(status(http.StatusTeapot))
	defer target.Close()

	source := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}),
	)
	defer source.Close()

	client, err := httpclient.New(testLogger(t), testConfig(t))
	require.NoError(t, err)

	resp, err := get(t, ctx, client, source.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusTeapot, resp.StatusCode)
}
