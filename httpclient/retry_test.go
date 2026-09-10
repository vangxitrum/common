package httpclient_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

func retryConfig(t *testing.T) httpclient.Config {
	t.Helper()
	config := testConfig(t)
	config.Retry = true
	config.RetryMax = 3
	return config
}

// Retry-After is the server telling you how long to wait, which is better
// information than any backoff can compute - so it replaces the backoff rather
// than being added to it.
func TestRetryHonorsRetryAfterSeconds(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		statusWithHeader(http.StatusServiceUnavailable, "Retry-After", "1"),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := httpclient.New(testLogger(t), retryConfig(t))
	require.NoError(t, err)

	clockCtx, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		resp, err := get(t, clockCtx, client, server.URL)
		if err != nil {
			return err
		}
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp.Body.Close()
	})

	// One second, because the server said so - not a draw from the 1s..30s
	// backoff, which under jitter would almost never be exactly this.
	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	ctx.Wait()

	require.Equal(t, 2, handler.calls())
}

func TestRetryHonorsRetryAfterDate(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	when := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	handler := newScript(
		statusWithHeader(http.StatusServiceUnavailable, "Retry-After", when),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := httpclient.New(testLogger(t), retryConfig(t))
	require.NoError(t, err)

	clockCtx, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		resp, err := get(t, clockCtx, client, server.URL)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, 3*time.Second))
	ctx.Wait()

	require.Equal(t, 2, handler.calls())
}

// Waiting as long as a remote asks is a way for whoever runs it to hold this
// service's goroutines. The response comes back instead, body intact.
func TestRetryAfterBeyondTheMaximumReturnsTheResponse(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		statusWithHeader(http.StatusServiceUnavailable, "Retry-After", "3600"),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	config := retryConfig(t)
	config.RetryWaitMax = 30 * time.Second

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(
		t,
		1,
		handler.calls(),
		"no second attempt, and no hour-long sleep",
	)
}

func TestWhichStatusesAreRetried(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status string
		code   int
		want   int
	}{
		"503 is transient":                     {code: http.StatusServiceUnavailable, want: 2},
		"500 is transient":                     {code: http.StatusInternalServerError, want: 2},
		"429 is backpressure":                  {code: http.StatusTooManyRequests, want: 2},
		"501 is a permanent statement":         {code: http.StatusNotImplemented, want: 1},
		"400 is the caller's own fault":        {code: http.StatusBadRequest, want: 1},
		"200 is not a failure":                 {code: http.StatusOK, want: 1},
		"418 is not a failure worth repeating": {code: http.StatusTeapot, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := testcontext.New(t)
			defer ctx.Cleanup()

			handler := newScript(status(tc.code), status(http.StatusOK))
			server := httptest.NewServer(handler)
			defer server.Close()

			config := retryConfig(t)
			config.RetryWaitMin = time.Millisecond
			config.RetryWaitMax = 2 * time.Millisecond

			client, err := httpclient.New(testLogger(t), config)
			require.NoError(t, err)

			resp, err := get(t, ctx, client, server.URL)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, tc.want, handler.calls())
		})
	}
}

// A POST that timed out may well have succeeded. Replaying it charges the card
// twice, so it takes an explicit statement that replaying is safe.
func TestWhichMethodsAreRetried(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		method         string
		idempotencyKey string
		want           int
	}{
		"GET is idempotent":               {method: http.MethodGet, want: 2},
		"PUT is idempotent":               {method: http.MethodPut, want: 2},
		"DELETE is idempotent":            {method: http.MethodDelete, want: 2},
		"POST is not":                     {method: http.MethodPost, want: 1},
		"PATCH is not":                    {method: http.MethodPatch, want: 1},
		"POST with a key says it is safe": {method: http.MethodPost, idempotencyKey: "k-1", want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := testcontext.New(t)
			defer ctx.Cleanup()

			handler := newScript(
				status(http.StatusServiceUnavailable),
				status(http.StatusOK),
			)
			server := httptest.NewServer(handler)
			defer server.Close()

			config := retryConfig(t)
			config.RetryWaitMin = time.Millisecond
			config.RetryWaitMax = 2 * time.Millisecond

			client, err := httpclient.New(testLogger(t), config)
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(
				ctx,
				tc.method,
				server.URL,
				strings.NewReader("body"),
			)
			require.NoError(t, err)
			if tc.idempotencyKey != "" {
				req.Header.Set("Idempotency-Key", tc.idempotencyKey)
			}

			resp, err := client.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, tc.want, handler.calls())
		})
	}
}

// Replay is free for the body types http.NewRequest can rewind, and this
// package never buffers one to pretend otherwise.
func TestBodyIsReplayedByteForByte(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	config := retryConfig(t)
	config.RetryWaitMin = time.Millisecond
	config.RetryWaitMax = 2 * time.Millisecond

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, server.URL,
		bytes.NewReader([]byte("the whole body")))
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 2, handler.calls())
	require.Equal(t, "the whole body", handler.call(0).body)
	require.Equal(
		t,
		"the whole body",
		handler.call(1).body,
		"the replay must send the same bytes",
	)
}

func TestBodyWithoutGetBodyIsNotRetried(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	config := retryConfig(t)
	config.RetryWaitMin = time.Millisecond
	config.RetryWaitMax = 2 * time.Millisecond

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	// An opaque reader: http.NewRequest cannot give it a GetBody, so there is
	// nothing to replay and nothing is buffered to invent one.
	body := io.NopCloser(strings.NewReader("stream"))
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		server.URL,
		body,
	)
	require.NoError(t, err)
	require.Nil(t, req.GetBody)

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 1, handler.calls())
}

func TestPerCallSiteOptIn(t *testing.T) {
	t.Parallel()

	t.Run("a non-retrying client retries when the call site asks", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		config := testConfig(t) // Retry stays false
		config.RetryMax = 3
		config.RetryWaitMin = time.Millisecond
		config.RetryWaitMax = 2 * time.Millisecond

		client, err := httpclient.New(testLogger(t), config)
		require.NoError(t, err)

		// A script counts its own calls, so each half of this needs its own.
		off := newScript(status(http.StatusServiceUnavailable), status(http.StatusOK))
		offServer := httptest.NewServer(off)
		defer offServer.Close()

		resp, err := get(t, ctx, client, offServer.URL)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 1, off.calls(), "off by default")

		on := newScript(status(http.StatusServiceUnavailable), status(http.StatusOK))
		onServer := httptest.NewServer(on)
		defer onServer.Close()

		resp, err = get(t, httpclient.WithRetry(ctx), client, onServer.URL)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 2, on.calls(), "and on when the call site says so")
	})

	t.Run(
		"a retrying client stops when the call site says not to",
		func(t *testing.T) {
			t.Parallel()

			ctx := testcontext.New(t)
			defer ctx.Cleanup()

			handler := newScript(
				status(http.StatusServiceUnavailable),
				status(http.StatusOK),
			)
			server := httptest.NewServer(handler)
			defer server.Close()

			config := retryConfig(t)
			config.RetryWaitMin = time.Millisecond
			config.RetryWaitMax = 2 * time.Millisecond

			client, err := httpclient.New(testLogger(t), config)
			require.NoError(t, err)

			resp, err := get(
				t,
				httpclient.WithoutRetry(ctx),
				client,
				server.URL,
			)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, 1, handler.calls())
		},
	)
}

// Every attempt at one request carries the same id, so a server-side dedupe key
// sees three tries at one request rather than three requests.
func TestRequestIDIsStableAcrossAttempts(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	config := retryConfig(t)
	config.RetryWaitMin = time.Millisecond
	config.RetryWaitMax = 2 * time.Millisecond

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(
		t,
		httpclient.WithRetry(httpclient.WithRequestID(ctx, "one-request")),
		client,
		server.URL,
	)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 2, handler.calls())
	require.Equal(
		t,
		"one-request",
		handler.call(0).header.Get(httpclient.RequestIDHeader),
	)
	require.Equal(
		t,
		"one-request",
		handler.call(1).header.Get(httpclient.RequestIDHeader),
	)
}

func TestRetryStopsOnACancelledContext(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusServiceUnavailable))
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := httpclient.New(testLogger(t), retryConfig(t))
	require.NoError(t, err)

	cancelable, cancel := context.WithCancel(ctx)
	clockCtx, machine := time2.WithNewMachine(cancelable)

	ctx.Go(func() error {
		resp, err := get(t, clockCtx, client, server.URL)
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
		return nil
	})

	require.True(t, machine.Block(ctx, 1))
	cancel()
	ctx.Wait()

	require.Equal(
		t,
		1,
		handler.calls(),
		"a cancellation must not buy another attempt",
	)
}

// The reason drain exists: a body closed unread costs the connection it arrived
// on, so a service retrying through an incident would also be destroying its
// own pool.
func TestDiscardedResponsesKeepTheConnection(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	// Comfortably under the drain limit, which is the case drain is for.
	errorPage := bytes.Repeat([]byte("x"), 512)
	handler := newScript(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(errorPage)
		},
		status(http.StatusOK),
	)

	counter := &connCounter{}
	server := httptest.NewUnstartedServer(handler)
	counter.track(server)
	server.Start()
	defer server.Close()

	config := retryConfig(t)
	config.RetryWaitMin = time.Millisecond
	config.RetryWaitMax = 2 * time.Millisecond

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 2, handler.calls())
	require.Equal(t, 1, counter.count(),
		"the retry must reuse the connection rather than open a second")
}

// The other side of the same decision, pinned so nobody "fixes" it: draining is
// bounded, so an error page larger than the limit is not read to the end and
// its connection is dropped. Copying a megabyte to io.Discard to save one
// handshake is the worse trade.
func TestAnOversizedDiscardedBodyIsNotFullyDrained(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	big := bytes.Repeat([]byte("x"), 1<<20)
	handler := newScript(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(big)
		},
		status(http.StatusOK),
	)

	counter := &connCounter{}
	server := httptest.NewUnstartedServer(handler)
	counter.track(server)
	server.Start()
	defer server.Close()

	config := retryConfig(t)
	config.RetryWaitMin = time.Millisecond
	config.RetryWaitMax = 2 * time.Millisecond

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 2, handler.calls())
	require.Equal(t, 2, counter.count())
}
