package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"math"

	// math/rand, not crypto/rand, on purpose: jitter only has to differ
	// between instances, not be unguessable. The module's no-math-rand lint
	// rule exempts this file for exactly this use.
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// Retryable decides whether a finished attempt is worth another.
//
// err is set when the attempt failed without an answer; resp is then usually
// nil. It must not read resp.Body: the body is either drained for the next
// attempt or handed back to the caller, and neither wants it half read.
type Retryable func(resp *http.Response, err error) bool

// Wait returns how long to sleep before the next attempt. attempt is the
// zero-based index of the attempt that just failed, and resp and err are what
// it produced.
type Wait func(attempt int, resp *http.Response, err error) time.Duration

const (
	// DefaultMaxAttempts is what a Retry with no MaxAttempts makes.
	DefaultMaxAttempts = 4

	// MaxRetryAfter is the longest Retry-After DefaultRetryable waits for.
	MaxRetryAfter = 30 * time.Second
)

// Retry is a caller's policy for one call site: how often, on what, and how
// long to wait in between.
//
// A client never retries on its own. A client that retries by default triples
// the load on a dependency that is already failing, and a replayed POST that
// timed out after succeeding charges the card twice - so retrying is something
// the caller does, where the knowledge of what is safe to repeat lives.
type Retry struct {
	// Name tags the retry metrics; empty means "http".
	Name string

	// MaxAttempts is the total number of attempts, the first included. Zero
	// means DefaultMaxAttempts, and one means no retry.
	MaxAttempts int

	// Retryable decides whether an attempt is worth another; nil means
	// DefaultRetryable.
	Retryable Retryable

	// Wait decides how long to sleep between attempts; nil means Retry-After
	// when the server sent one, and Exponential(time.Second, 30*time.Second)
	// otherwise.
	Wait Wait

	// AllowNonIdempotent retries a POST or PATCH that carries no
	// Idempotency-Key. Without it such a request gets exactly one attempt,
	// because a POST that timed out may well have succeeded.
	AllowNonIdempotent bool

	// Log, when set, gets one warn line per retry.
	Log *slog.Logger
}

// Do calls the client until an attempt is not worth repeating, the attempts
// run out, or ctx ends.
//
// newRequest builds the request for each attempt, so a body is sent whole every
// time and nothing is buffered to replay it. What comes back is the last
// attempt's response and error, exactly as the client returned them: out of
// attempts, the caller reads the final 503 rather than an error that hides it.
// A response that was superseded by another attempt is drained, which keeps its
// connection in the pool.
//
// The first attempt's Idempotency-Key is sent on every retry, whatever
// newRequest builds later. A caller that generates the key inside newRequest
// would otherwise send a new one each time, and the server would take the
// retry of a POST that timed out after succeeding as a second request.
//
// Every attempt shares ctx, so a deadline on it covers them all, and a wait
// that cannot be followed by an attempt before the deadline is not slept.
func (r Retry) Do(
	ctx context.Context,
	client *http.Client,
	newRequest func(ctx context.Context) (*http.Request, error),
) (*http.Response, error) {
	maxAttempts := r.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	retryable := r.Retryable
	if retryable == nil {
		retryable = DefaultRetryable
	}
	wait := r.Wait
	if wait == nil {
		wait = RetryAfter(Exponential(time.Second, 30*time.Second))
	}
	name := monkit.NewSeriesTag("name", r.name())

	var key string
	for attempt := 0; ; attempt++ {
		req, err := newRequest(ctx)
		if err != nil {
			return nil, err
		}
		if attempt == 0 {
			key = req.Header.Get("Idempotency-Key")
			if !r.replayable(req) {
				maxAttempts = 1
			}
		} else if key != "" {
			// The server has seen the first key; only that one makes this a
			// replay rather than a new request.
			req.Header.Set("Idempotency-Key", key)
		}

		//nolint:bodyclose // a superseded response is drained below; the last one belongs to the caller
		resp, err := client.Do(req)

		// The caller giving up is never a reason to try again, and only the
		// context can say so: a per-attempt timeout looks the same in err.
		if ctx.Err() != nil || !retryable(resp, err) {
			return resp, err
		}
		if attempt+1 >= maxAttempts {
			if maxAttempts > 1 {
				mon.Counter("http_retries_exhausted", name).Inc(1)
			}
			return resp, err
		}

		delay := max(wait(attempt, resp, err), 0)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return resp, err
		}

		drain(resp)
		if !time2.Sleep(ctx, delay) {
			// Handing back a response now would answer a request the caller
			// abandoned.
			return nil, Error.Wrap(ctx.Err())
		}

		mon.Counter("http_retries", name).Inc(1)
		if r.Log != nil {
			// The host and the method, never the URL: a path carries ids and
			// a query string carries tokens.
			r.Log.WarnContext(ctx, "retrying an http request",
				"client", name.Val, "method", req.Method,
				"host", req.URL.Host, "attempt", attempt+2)
		}
	}
}

func (r Retry) name() string {
	if r.Name == "" {
		return "http"
	}
	return r.Name
}

// replayable reports whether the request may be sent more than once.
func (r Retry) replayable(req *http.Request) bool {
	return idempotent(req.Method) ||
		req.Header.Get("Idempotency-Key") != "" ||
		r.AllowNonIdempotent
}

// DefaultRetryable retries what may pass: a connection error that is not a
// certificate failure or a blocked address, a 429, and a 5xx other than 501.
// It gives up on a Retry-After longer than MaxRetryAfter.
func DefaultRetryable(resp *http.Response, err error) bool {
	return retryable(resp, err, MaxRetryAfter)
}

// RetryableWithin is DefaultRetryable with a different Retry-After limit.
func RetryableWithin(limit time.Duration) Retryable {
	return func(resp *http.Response, err error) bool {
		return retryable(resp, err, limit)
	}
}

func retryable(resp *http.Response, err error, limit time.Duration) bool {
	if err != nil {
		return retryableError(err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
	case resp.StatusCode == http.StatusNotImplemented:
		// A permanent statement about the endpoint. Retrying it is guaranteed
		// waste.
		return false
	case resp.StatusCode < 500:
		return false
	}

	// Waiting as long as a remote asks is a way for whoever runs that remote to
	// hold this service's goroutines and its pooled connections.
	if wait, ok := ParseRetryAfter(resp); ok && wait > limit {
		return false
	}
	return true
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

	if errors.Is(err, ErrBlockedAddress) {
		// Refused by address, and it will be the same address next time.
		return false
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Our own redirect policy refusing is not transient either.
		if urlErr.Err != nil && errors.Is(urlErr.Err, errRedirect) {
			return false
		}
	}
	return true
}

// ParseRetryAfter reads how long a 429 or a 503 asked the caller to wait.
//
// Both forms RFC 9110 allows are accepted: a count of seconds, and an HTTP
// date. ok is false when there is no usable instruction.
func ParseRetryAfter(resp *http.Response) (wait time.Duration, ok bool) {
	if resp == nil {
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

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int(math.MaxInt64/int64(time.Second)) {
			// Past what a Duration holds, the multiplication wraps - to -1s
			// for the largest int, which would turn "wait forever" into "retry
			// now". Saturate instead, so a Retryable sees it as too long.
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}

	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(time.Until(when), 0), true
}

// RetryAfter waits as long as the server's Retry-After asked, and as long as
// fallback says when it asked nothing.
//
// It obeys the header whatever its size. Bounding it is the Retryable's job,
// which is why DefaultRetryable gives up on a Retry-After past MaxRetryAfter.
func RetryAfter(fallback Wait) Wait {
	return func(attempt int, resp *http.Response, err error) time.Duration {
		if wait, ok := ParseRetryAfter(resp); ok {
			return wait
		}
		if fallback == nil {
			return 0
		}
		return fallback(attempt, resp, err)
	}
}

// Exponential waits a random time below a nominal wait that starts at minWait
// and doubles after each attempt, up to maxWait.
//
// The randomness is the point. When a dependency restarts, every instance that
// was talking to it fails in the same instant; without jitter they all retry in
// the same instant too, through every doubling. Full jitter - uniform on [0,
// nominal) - breaks that up, at the cost of an expected wait half the nominal
// one.
func Exponential(minWait, maxWait time.Duration) Wait {
	maxWait = max(maxWait, minWait)
	return func(attempt int, _ *http.Response, _ error) time.Duration {
		nominal := minWait
		for i := 0; i < attempt && nominal > 0 && nominal < maxWait; i++ {
			nominal *= 2
		}
		// A doubling past maxWait, or one that overflowed.
		if nominal > maxWait || nominal < 0 {
			nominal = maxWait
		}
		// The top-level math/rand/v2 functions keep per-P state and take no
		// global lock, which matters when a hundred goroutines back off at once.
		return time.Duration(rand.Float64() * float64(nominal))
	}
}

// Constant waits d between every pair of attempts.
func Constant(d time.Duration) Wait {
	return func(int, *http.Response, error) time.Duration { return d }
}
