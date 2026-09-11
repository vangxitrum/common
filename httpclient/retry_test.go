package httpclient_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// fastRetry is a policy whose waits are too short to slow a test down, so a
// test about what is retried does not have to drive a clock.
func fastRetry(t *testing.T) httpclient.Retry {
	t.Helper()
	return httpclient.Retry{
		Name:        t.Name(),
		MaxAttempts: 3,
		Wait:        httpclient.Constant(time.Millisecond),
		Log:         testLogger(t),
	}
}

// doGet runs a GET through the policy, building a fresh request per attempt the
// way a caller is meant to.
func doGet(
	t *testing.T,
	ctx context.Context,
	policy httpclient.Retry,
	client *http.Client,
	url string,
) (*http.Response, error) {
	t.Helper()
	return policy.Do(
		ctx,
		client,
		func(ctx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		},
	)
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := httpclient.New(testLogger(t), testConfig(t))
	require.NoError(t, err)
	return client
}

func TestRetryRetriesUntilASuccess(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusBadGateway),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := doGet(t, ctx, fastRetry(t), newClient(t), server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 3, handler.calls())
}

// A client makes one attempt per call. Retrying is the caller's decision, taken
// by calling Retry.Do, never something the client does behind its back.
func TestAPlainClientDoesNotRetry(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := get(t, ctx, newClient(t), server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, 1, handler.calls())
}

func TestWhichStatusesAreRetried(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		code int
		want int
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

			resp, err := doGet(t, ctx, fastRetry(t), newClient(t), server.URL)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, tc.want, handler.calls())
		})
	}
}

func TestDefaultRetryableOnErrors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"a refused connection may pass": {
			err:  &url.Error{Op: "Get", URL: "http://x", Err: syscall.ECONNREFUSED},
			want: true,
		},
		"a certificate will not verify in a second": {
			err:  &url.Error{Op: "Get", URL: "https://x", Err: x509.UnknownAuthorityError{}},
			want: false,
		},
		"a blocked address is blocked next time too": {
			err:  &url.Error{Op: "Get", URL: "http://x", Err: httpclient.ErrBlockedAddress},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, httpclient.DefaultRetryable(nil, tc.err))
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
		allow          bool
		want           int
	}{
		"GET is idempotent":               {method: http.MethodGet, want: 2},
		"PUT is idempotent":               {method: http.MethodPut, want: 2},
		"DELETE is idempotent":            {method: http.MethodDelete, want: 2},
		"POST is not":                     {method: http.MethodPost, want: 1},
		"PATCH is not":                    {method: http.MethodPatch, want: 1},
		"POST with a key says it is safe": {method: http.MethodPost, idempotencyKey: "k-1", want: 2},
		"POST the caller vouches for":     {method: http.MethodPost, allow: true, want: 2},
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

			policy := fastRetry(t)
			policy.AllowNonIdempotent = tc.allow

			resp, err := policy.Do(
				ctx,
				newClient(t),
				func(ctx context.Context) (*http.Request, error) {
					req, err := http.NewRequestWithContext(
						ctx,
						tc.method,
						server.URL,
						strings.NewReader("body"),
					)
					if err != nil {
						return nil, err
					}
					if tc.idempotencyKey != "" {
						req.Header.Set("Idempotency-Key", tc.idempotencyKey)
					}
					return req, nil
				},
			)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, tc.want, handler.calls())
		})
	}
}

// The caller knows its dependency: a 409 from this one means "busy, try again",
// which no default can know.
func TestACustomRetryableDecides(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusConflict), status(http.StatusOK))
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	policy.Retryable = func(resp *http.Response, err error) bool {
		if resp != nil && resp.StatusCode == http.StatusConflict {
			return true
		}
		return httpclient.DefaultRetryable(resp, err)
	}

	resp, err := doGet(t, ctx, policy, newClient(t), server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, handler.calls())
}

// Wait is told which attempt failed and how, and the sleep is exactly what it
// returned.
func TestWaitIsAskedAfterEachFailure(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusBadGateway),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	type seen struct {
		attempt int
		status  int
	}
	var (
		mu    sync.Mutex
		asked []seen
	)

	policy := fastRetry(t)
	policy.Wait = func(attempt int, resp *http.Response, _ error) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, seen{attempt: attempt, status: resp.StatusCode})
		return time.Duration(attempt+1) * 5 * time.Second
	}

	clockCtx, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		resp, err := doGet(t, clockCtx, policy, newClient(t), server.URL)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, 5*time.Second))
	require.True(t, machine.BlockThenAdvance(ctx, 1, 10*time.Second))
	ctx.Wait()

	require.Equal(t, 3, handler.calls())
	require.Equal(t, []seen{
		{attempt: 0, status: http.StatusServiceUnavailable},
		{attempt: 1, status: http.StatusBadGateway},
	}, asked)
}

// Retry-After is the server telling you how long to wait, which is better
// information than any backoff can compute - so it replaces the fallback.
func TestRetryAfterHonorsSeconds(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		statusWithHeader(http.StatusServiceUnavailable, "Retry-After", "1"),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	// A fallback no test would ever advance past, so a pass proves the header
	// was used.
	policy.Wait = httpclient.RetryAfter(httpclient.Constant(time.Hour))

	clockCtx, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		resp, err := doGet(t, clockCtx, policy, newClient(t), server.URL)
		if err != nil {
			return err
		}
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp.Body.Close()
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Second))
	ctx.Wait()

	require.Equal(t, 2, handler.calls())
}

func TestRetryAfterHonorsADate(t *testing.T) {
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

	policy := fastRetry(t)
	policy.Wait = httpclient.RetryAfter(httpclient.Constant(time.Hour))

	clockCtx, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		resp, err := doGet(t, clockCtx, policy, newClient(t), server.URL)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, 3*time.Second))
	ctx.Wait()

	require.Equal(t, 2, handler.calls())
}

func TestRetryAfterFallsBackWithoutTheHeader(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	policy.Wait = httpclient.RetryAfter(httpclient.Constant(2 * time.Second))

	clockCtx, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		resp, err := doGet(t, clockCtx, policy, newClient(t), server.URL)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, 2*time.Second))
	ctx.Wait()

	require.Equal(t, 2, handler.calls())
}

// Waiting as long as a remote asks is a way for whoever runs it to hold this
// service's goroutines. The default gives up instead and hands the response
// back, body intact.
func TestRetryAfterBeyondTheLimitReturnsTheResponse(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("come back in an hour"))
		},
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	policy.Wait = httpclient.RetryAfter(httpclient.Constant(time.Millisecond))

	resp, err := doGet(t, ctx, policy, newClient(t), server.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "come back in an hour", string(body))
	require.Equal(
		t,
		1,
		handler.calls(),
		"no second attempt, and no hour-long sleep",
	)
}

func TestRetryableWithinRaisesTheLimit(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		statusWithHeader(http.StatusServiceUnavailable, "Retry-After", "3600"),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	policy.Retryable = httpclient.RetryableWithin(2 * time.Hour)
	policy.Wait = httpclient.RetryAfter(httpclient.Constant(time.Millisecond))

	clockCtx, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		resp, err := doGet(t, clockCtx, policy, newClient(t), server.URL)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, time.Hour))
	ctx.Wait()

	require.Equal(t, 2, handler.calls())
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		code   int
		header string
		want   time.Duration
		ok     bool
	}{
		"seconds on a 503":       {code: http.StatusServiceUnavailable, header: "7", want: 7 * time.Second, ok: true},
		"seconds on a 429":       {code: http.StatusTooManyRequests, header: "2", want: 2 * time.Second, ok: true},
		"a date in the past":     {code: http.StatusServiceUnavailable, header: "Mon, 02 Jan 2006 15:04:05 GMT", want: 0, ok: true},
		"no header":              {code: http.StatusServiceUnavailable},
		"garbage":                {code: http.StatusServiceUnavailable, header: "soon"},
		"negative seconds":       {code: http.StatusServiceUnavailable, header: "-1"},
		"ignored on another 5xx": {code: http.StatusInternalServerError, header: "7"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{StatusCode: tc.code, Header: http.Header{}}
			if tc.header != "" {
				resp.Header.Set("Retry-After", tc.header)
			}

			got, ok := httpclient.ParseRetryAfter(resp)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}

	_, ok := httpclient.ParseRetryAfter(nil)
	require.False(t, ok, "a failed attempt has no response to read")
}

// Full jitter: every wait is drawn from [0, nominal), and the nominal wait
// doubles from min up to max.
func TestExponentialStaysUnderItsNominalWait(t *testing.T) {
	t.Parallel()

	wait := httpclient.Exponential(time.Second, 8*time.Second)

	for attempt, nominal := range []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second,
	} {
		for range 200 {
			got := wait(attempt, nil, nil)
			require.GreaterOrEqual(t, got, time.Duration(0))
			require.Less(t, got, nominal, "attempt %d", attempt)
		}
	}

	// Far past any real attempt count, which must clamp rather than overflow
	// into a negative or garbage duration.
	for range 200 {
		got := wait(10_000, nil, nil)
		require.GreaterOrEqual(t, got, time.Duration(0))
		require.Less(t, got, 8*time.Second)
	}
}

func TestConstantWaitsTheSameEveryTime(t *testing.T) {
	t.Parallel()

	wait := httpclient.Constant(3 * time.Second)
	require.Equal(t, 3*time.Second, wait(0, nil, nil))
	require.Equal(t, 3*time.Second, wait(9, nil, nil))
}

// Every attempt is built fresh, so a body is sent whole every time rather than
// once and then as an already drained reader.
func TestTheBodyIsSentOnEveryAttempt(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	var built int
	resp, err := fastRetry(
		t,
	).Do(ctx, newClient(t), func(ctx context.Context) (*http.Request, error) {
		built++
		// An opaque reader, which http.NewRequest cannot rewind - the case a
		// transport-level retry could not replay at all.
		body := io.NopCloser(strings.NewReader("the whole body"))
		return http.NewRequestWithContext(ctx, http.MethodPut, server.URL, body)
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 2, built)
	require.Equal(t, 2, handler.calls())
	require.Equal(t, "the whole body", handler.call(0).body)
	require.Equal(
		t,
		"the whole body",
		handler.call(1).body,
		"the retry must send the same bytes",
	)
}

func TestARequestThatCannotBeBuiltIsNotRetried(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	broken := errors.New("no such tenant")
	var built int
	policy := fastRetry(t)
	//nolint:bodyclose // no request is ever sent, so there is no body
	resp, err := policy.Do(ctx, newClient(t), func(context.Context) (*http.Request, error) {
		built++
		return nil, broken
	})
	require.Nil(t, resp)
	require.ErrorIs(t, err, broken)
	require.Equal(t, 1, built)
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

	resp, err := doGet(
		t,
		httpclient.WithRequestID(ctx, "one-request"),
		fastRetry(t),
		newClient(t),
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

// Out of attempts, the caller gets the last response to read, not an error that
// hides the status behind it.
func TestExhaustionReturnsTheLastResponse(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("still down"))
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)

	resp, err := doGet(t, ctx, policy, newClient(t), server.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, 3, handler.calls())
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "still down", string(body))

	require.Equal(
		t,
		2.0,
		seriesValue(t, "http_retries", policy.Name, "value", nil),
	)
	require.Equal(
		t,
		1.0,
		seriesValue(t, "http_retries_exhausted", policy.Name, "value", nil),
	)
}

func TestOneAttemptMeansNoRetry(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	policy.MaxAttempts = 1

	resp, err := doGet(t, ctx, policy, newClient(t), server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 1, handler.calls())
}

func TestRetryStopsOnACancelledContext(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusServiceUnavailable))
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	policy.Wait = httpclient.Constant(time.Second)

	cancelable, cancel := context.WithCancel(ctx)
	clockCtx, machine := time2.WithNewMachine(cancelable)

	ctx.Go(func() error {
		//nolint:bodyclose // the assertion is that there is no response at all
		resp, err := doGet(t, clockCtx, policy, newClient(t), server.URL)
		require.Nil(t, resp, "an abandoned request has no answer to hand back")
		require.ErrorIs(t, err, context.Canceled)
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

// A wait that cannot be followed by an attempt before the deadline is not slept
// at all: the caller gets the real answer now rather than a bare deadline
// exceeded later.
func TestAWaitPastTheDeadlineIsNotSlept(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(
		status(http.StatusServiceUnavailable),
		status(http.StatusOK),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	policy := fastRetry(t)
	policy.Wait = httpclient.Constant(time.Hour)

	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := doGet(t, bounded, policy, newClient(t), server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, 1, handler.calls())
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

	resp, err := doGet(t, ctx, fastRetry(t), newClient(t), server.URL)
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

	resp, err := doGet(t, ctx, fastRetry(t), newClient(t), server.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 2, handler.calls())
	require.Equal(t, 2, counter.count())
}
