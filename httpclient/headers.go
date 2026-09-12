package httpclient

import (
	"context"
	"net/http"
)

// RequestIDHeader is the header a request id travels in.
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// WithRequestID puts a request id on the context, so every request made with
// that context carries it.
//
// It is read from the context, and Retry.Do runs every attempt under one
// context, so every attempt at one request sends the same id. That is what makes a server-side dedupe or idempotency key
// see three tries at one request rather than three unrelated requests.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the id set by [WithRequestID], or the empty string.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestID copies an id from the context onto the request.
func requestIDTransport(next http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		id := RequestID(req.Context())
		if id == "" || req.Header.Get(RequestIDHeader) != "" {
			return next.RoundTrip(req)
		}

		// The RoundTripper contract forbids modifying the request, and
		// violating it produces bugs that only show up under concurrency.
		clone := req.Clone(req.Context())
		clone.Header.Set(RequestIDHeader, id)
		return next.RoundTrip(clone)
	})
}

// userAgent stamps the build's identity on requests that do not already carry
// one.
//
// A request that has set User-Agent keeps it, so an SDK layered over this
// client keeps its own identity. A request that set it to the empty string
// meant to suppress the header - Go's own convention - and keeps that too,
// which is why this checks for the header's presence rather than its value.
func userAgentTransport(
	agent string,
	next http.RoundTripper,
) http.RoundTripper {
	if agent == "" {
		return next
	}

	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if _, ok := req.Header[http.CanonicalHeaderKey("User-Agent")]; ok {
			return next.RoundTrip(req)
		}

		clone := req.Clone(req.Context())
		clone.Header.Set("User-Agent", agent)
		return next.RoundTrip(clone)
	})
}
