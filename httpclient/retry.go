package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/retry"
)

type retryKey struct{}

// WithRetry opts one call site into retrying, on a client that does not retry
// by default.
//
// The layer is always installed and always reads the context, so a single
// shared client - and a single connection pool - serves both the calls that
// should retry and the calls that must not.
func WithRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryKey{}, true)
}

// WithoutRetry opts one call site out of retrying, on a client that retries by
// default.
func WithoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryKey{}, false)
}

func retryWanted(ctx context.Context, byDefault bool) bool {
	if wanted, ok := ctx.Value(retryKey{}).(bool); ok {
		return wanted
	}
	return byDefault
}

// retryTransport replays a request that failed in a way worth replaying.
//
// It is a RoundTripper rather than a wrapper around Client.Do for one reason
// that decides everything else: New returns an *http.Client, so it has to work
// inside any vendored SDK that takes one. A RoundTripper also sees
// req.GetBody, which http.NewRequest fills in for the common body types, so a
// replay costs nothing and no body is ever buffered up front.
//
// The cost is that it sits inside Client.Timeout and inside the caller's
// deadline, so every attempt shares one budget. That is correct - the deadline
// is the caller's statement of how long it is willing to wait - but it means a
// retrying call site has to set a deadline that covers all the attempts.
type retryTransport struct {
	next    http.RoundTripper
	log     *slog.Logger
	metrics *metrics

	byDefault     bool
	nonIdempotent bool
	maxAttempts   int
	waitMax       time.Duration
	backoff       *retry.Backoff
	name          monkit.SeriesTag
}

func newRetryTransport(
	config Config,
	log *slog.Logger,
	m *metrics,
	next http.RoundTripper,
) *retryTransport {
	return &retryTransport{
		next:    next,
		log:     log,
		metrics: m,

		byDefault:     config.Retry,
		nonIdempotent: config.RetryNonIdempotent,
		maxAttempts:   config.RetryMax + 1,
		waitMax:       config.RetryWaitMax,
		backoff: retry.New(retry.Config{
			Name:         config.Name,
			MaxAttempts:  config.RetryMax + 1,
			InitialDelay: config.RetryWaitMin,
			MaxDelay:     config.RetryWaitMax,
			Multiplier:   2,
			Jitter:       true,
		}),
		name: monkit.NewSeriesTag("name", config.Name),
	}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.shouldRetry(req) {
		return t.next.RoundTrip(req)
	}

	loop := t.backoff.Loop()
	var (
		resp *http.Response
		err  error
	)

	for loop.Wait(req.Context()) {
		if resp != nil {
			// The previous attempt has been superseded, so drain it. A body
			// closed unread costs the connection it arrived on, which is the
			// last thing a service retrying through an incident can afford.
			drain(resp)

			t.metrics.retries.Inc(1)
			mon.Event("http_retry", t.name)

			if t.log != nil {
				// The host and the method, never the URL: a path carries ids
				// and a query string carries tokens.
				t.log.Warn("retrying an http request",
					"client", t.name.Val, "method", req.Method,
					"host", req.URL.Host, "attempt", loop.Attempt())
			}
		}

		attempt, bodyErr := t.attemptRequest(req)
		if bodyErr != nil {
			return nil, bodyErr
		}

		//nolint:bodyclose // a superseded response is drained at the top of the next turn; the last one belongs to the caller
		resp, err = t.next.RoundTrip(attempt)
		if !t.retryable(attempt, resp, err) {
			return resp, err
		}

		wait, ok := retryAfter(resp, err)
		if !ok {
			continue // no instruction, so the computed backoff applies
		}
		if wait > t.waitMax {
			// Waiting as long as a remote asks is a way for whoever runs that
			// remote to hold this service's goroutines and its pooled
			// connections. Hand the response back instead, body intact, and
			// let the caller decide what to do about it.
			mon.Event("http_retry_after_too_long", t.name)
			return resp, err
		}
		loop.Override(wait)
	}

	if errors.Is(loop.Err(), retry.ErrExhausted) {
		t.metrics.exhausts.Inc(1)
		mon.Event("http_retry_exhausted", t.name)
	}

	// The caller stopped us mid-sequence. Handing back the response we happened
	// to be holding would answer a request that was abandoned, so drain it and
	// report the cancellation, which is what any other transport would do.
	if ctxErr := req.Context().Err(); ctxErr != nil {
		drain(resp)
		return nil, Error.Wrap(ctxErr)
	}

	// The final attempt was never drained, so the caller gets a readable
	// response rather than an error that hides the status behind it.
	if resp == nil && err == nil {
		return nil, Error.Wrap(req.Context().Err())
	}
	return resp, err
}

// attemptRequest clones the request and rewinds its body.
//
// The caller's request is never modified: the RoundTripper contract forbids it,
// and violations only show up under concurrency.
func (t *retryTransport) attemptRequest(
	req *http.Request,
) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.GetBody == nil {
		return clone, nil
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, Error.Errorf("rewinding the request body: %w", err)
	}
	clone.Body = body
	return clone, nil
}

// shouldRetry reports whether this request may be replayed at all.
func (t *retryTransport) shouldRetry(req *http.Request) bool {
	if t.maxAttempts <= 1 || !retryWanted(req.Context(), t.byDefault) {
		return false
	}

	// A body from an arbitrary io.Reader cannot be rewound, and this package
	// does not buffer one to pretend otherwise.
	if req.Body != nil && req.GetBody == nil {
		return false
	}

	if idempotent(req.Method) || req.Header.Get("Idempotency-Key") != "" {
		return true
	}
	return t.nonIdempotent
}

// retryable decides whether this attempt is worth another.
func (t *retryTransport) retryable(
	req *http.Request,
	resp *http.Response,
	err error,
) bool {
	// The caller giving up is never a reason to try again, and only the
	// context can say that: an inner timeout looks identical in the error.
	if req.Context().Err() != nil {
		return false
	}

	if err != nil {
		return retryableError(err)
	}

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusNotImplemented:
		// A permanent statement about the endpoint. Retrying it is guaranteed
		// waste.
		return false
	default:
		return resp.StatusCode >= 500
	}
}

// retryableError separates a fault that may pass from one that never will.
func retryableError(err error) bool {
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostnameErr) {
		// A certificate that does not verify will not verify in a second.
		return false
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// A bad scheme, a missing host, or our own redirect policy refusing.
		// None of them is transient.
		if urlErr.Err != nil && errors.Is(urlErr.Err, errRedirect) {
			return false
		}
	}
	return true
}

// retryAfter reads the header the server used to say how long to wait.
//
// Both forms RFC 9110 allows are accepted: a count of seconds, and an HTTP
// date. A server that says nothing gets the computed backoff instead.
func retryAfter(resp *http.Response, err error) (time.Duration, bool) {
	if err != nil || resp == nil {
		return 0, false
	}
	if resp.StatusCode != http.StatusTooManyRequests &&
		resp.StatusCode != http.StatusServiceUnavailable {
		return 0, false
	}

	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0, false
	}

	if seconds, convErr := strconv.Atoi(value); convErr == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}

	when, convErr := http.ParseTime(value)
	if convErr != nil {
		return 0, false
	}
	wait := time.Until(when)
	if wait <= 0 {
		return 0, true
	}
	return wait, true
}
