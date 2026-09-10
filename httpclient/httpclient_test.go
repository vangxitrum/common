package httpclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

func get(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	url string,
) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	return client.Do(req) //nolint:bodyclose // the caller closes what it gets
}

// The failure this package exists for: a dependency that accepts a connection
// and then never answers must not hold the caller forever.
func TestTimeoutFires(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	release := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-release
		}),
	)
	defer server.Close()
	defer close(release)

	config := testConfig(t)
	config.Timeout = 100 * time.Millisecond

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.Error(t, err)
	if resp != nil {
		_ = resp.Body.Close()
	}

	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr)
	require.True(t, urlErr.Timeout(), "the failure must present as a timeout")
}

// The timeout is a backstop. A context deadline is the real bound, and it wins.
func TestContextDeadlineBeatsTheTimeout(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	release := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-release
		}),
	)
	defer server.Close()
	defer close(release)

	client, err := httpclient.New(testLogger(t), testConfig(t))
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	resp, err := get(t, deadline, client, server.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestUserAgent(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusOK))
	server := httptest.NewServer(handler)
	defer server.Close()

	t.Run("a derived agent names the binary", func(t *testing.T) {
		client, err := httpclient.New(testLogger(t), testConfig(t))
		require.NoError(t, err)

		resp, err := get(t, ctx, client, server.URL)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		agent := handler.call(handler.calls() - 1).header.Get("User-Agent")
		require.NotEmpty(t, agent)
		require.NotContains(t, agent, "Go-http-client")
	})

	t.Run("the config overrides it", func(t *testing.T) {
		config := testConfig(t)
		config.UserAgent = "billing/9.9.9"

		client, err := httpclient.New(testLogger(t), config)
		require.NoError(t, err)

		resp, err := get(t, ctx, client, server.URL)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(
			t,
			"billing/9.9.9",
			handler.call(handler.calls()-1).header.Get("User-Agent"),
		)
	})

	t.Run("a request that set one keeps it", func(t *testing.T) {
		client, err := httpclient.New(testLogger(t), testConfig(t))
		require.NoError(t, err)

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			server.URL,
			nil,
		)
		require.NoError(t, err)
		req.Header.Set("User-Agent", "sdk/1.0")

		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(
			t,
			"sdk/1.0",
			handler.call(handler.calls()-1).header.Get("User-Agent"),
		)
	})

	// Go's own convention: an explicitly empty User-Agent suppresses the
	// header. Setting one over the top would break a deliberate choice.
	t.Run("an explicitly empty one stays suppressed", func(t *testing.T) {
		client, err := httpclient.New(testLogger(t), testConfig(t))
		require.NoError(t, err)

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			server.URL,
			nil,
		)
		require.NoError(t, err)
		req.Header.Set("User-Agent", "")

		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Empty(
			t,
			handler.call(handler.calls()-1).header.Get("User-Agent"),
		)
	})
}

func TestRequestID(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusOK))
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := httpclient.New(testLogger(t), testConfig(t))
	require.NoError(t, err)

	resp, err := get(
		t,
		httpclient.WithRequestID(ctx, "abc-123"),
		client,
		server.URL,
	)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(
		t,
		"abc-123",
		handler.call(0).header.Get(httpclient.RequestIDHeader),
	)
	require.Equal(
		t,
		"abc-123",
		httpclient.RequestID(httpclient.WithRequestID(ctx, "abc-123")),
	)
}

// A pooled client reuses its connection; a one-shot client must not, because a
// pooled transport kept for transient destinations leaks file descriptors.
func TestOneShotDoesNotPool(t *testing.T) {
	t.Parallel()

	for name, oneShot := range map[string]bool{"pooled": false, "one-shot": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The context belongs to the subtest: a parallel subtest runs
			// after its parent returns, so a parent-scoped one is already
			// canceled by the time the request goes out.
			ctx := testcontext.New(t)
			defer ctx.Cleanup()

			counter := &connCounter{}
			server := httptest.NewUnstartedServer(
				newScript(status(http.StatusOK)),
			)
			counter.track(server)
			server.Start()
			defer server.Close()

			config := testConfig(t)
			build := httpclient.New
			if oneShot {
				build = httpclient.NewOneShot
				// The server is on loopback, which the one-shot profile
				// refuses by default; this test is about pooling.
				config.AllowedNetworks = []string{"127.0.0.0/8"}
			}
			client, err := build(testLogger(t), config)
			require.NoError(t, err)

			for range 2 {
				resp, err := get(t, ctx, client, server.URL)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}

			if oneShot {
				require.Equal(t, 2, counter.count(), "keep-alives must be off")
			} else {
				require.Equal(
					t,
					1,
					counter.count(),
					"the connection must be reused",
				)
			}
		})
	}
}

func TestNewWithAppliesTheChain(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	var seen *http.Request
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seen = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     http.Header{},
			Request:    req,
		}, nil
	})

	config := testConfig(t)
	config.UserAgent = "custom/1.0"

	client, err := httpclient.NewWith(testLogger(t), config, base)
	require.NoError(t, err)

	resp, err := get(
		t,
		httpclient.WithRequestID(ctx, "id-9"),
		client,
		"http://example.invalid/x",
	)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, "custom/1.0", seen.Header.Get("User-Agent"))
	require.Equal(t, "id-9", seen.Header.Get(httpclient.RequestIDHeader))
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*httpclient.Config){
		"an unsupported tls version": func(c *httpclient.Config) { c.TLSMinVersion = "1.1" },
		"a negative timeout":         func(c *httpclient.Config) { c.Timeout = -time.Second },
		"a negative redirect cap":    func(c *httpclient.Config) { c.MaxRedirects = -1 },
		"an inverted retry window": func(c *httpclient.Config) {
			c.RetryWaitMin, c.RetryWaitMax = time.Minute, time.Second
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			config := testConfig(t)
			mutate(&config)

			_, err := httpclient.New(testLogger(t), config)
			require.Error(
				t,
				err,
				"the failure belongs at construction, next to the configuration",
			)
		})
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	t.Parallel()

	_, err := httpclient.New(nil, testConfig(t))
	require.Error(t, err)

	_, err = httpclient.NewWith(testLogger(t), testConfig(t), nil)
	require.Error(t, err)
}

// A caller may still build the transport and adjust one field.
func TestTransportIsExported(t *testing.T) {
	t.Parallel()

	transport, err := httpclient.Transport(testConfig(t))
	require.NoError(t, err)
	require.Positive(
		t,
		transport.MaxIdleConnsPerHost,
		"the standard library's two is the bug",
	)
	require.False(t, transport.DisableKeepAlives)
	require.NotNil(t, transport.Proxy, "an unguarded transport honors HTTP_PROXY")

	oneShot, err := httpclient.OneShotTransport(testConfig(t))
	require.NoError(t, err)
	require.True(t, oneShot.DisableKeepAlives)
	require.Equal(t, -1, oneShot.MaxIdleConnsPerHost)
	require.Nil(t, oneShot.Proxy, "a proxy would dial on the guard's behalf")
}

func TestErrorsCarryNoPathOrQuery(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	config := testConfig(t)
	config.MaxRedirects = 1

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/next?token=super-secret", http.StatusFound)
		}),
	)
	defer server.Close()

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)

	// url.Error prints the URL the caller passed, which is stdlib behavior and
	// out of this package's hands. What must not appear is anything this
	// package added: the redirect target's query string.
	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr)
	require.NotContains(t, urlErr.Err.Error(), "super-secret")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	return fn(req)
}
