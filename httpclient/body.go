package httpclient

import (
	"io"
	"net/http"

	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
)

// drainLimit is how much of a discarded body is read before it is closed.
const drainLimit = 4096

// bodyLimitTransport caps how much of a response body a caller can read.
//
// It is the innermost layer, which is the only position where nothing above it
// can have replaced the body already.
func bodyLimitTransport(
	limit memory.Size,
	capped func(),
	next http.RoundTripper,
) http.RoundTripper {
	if limit <= 0 {
		return next
	}

	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(req)
		if err != nil || resp.Body == nil {
			return resp, err
		}

		resp.Body = &limitedBody{
			inner:  resp.Body,
			limit:  limit,
			capped: capped,
		}
		return resp, nil
	})
}

// limitedBody fails once more than limit bytes have been read.
//
// It deliberately does not wrap io.LimitReader, which would end an over-long
// body in io.EOF: a forty megabyte response truncated at thirty-two would reach
// the caller as "unexpected end of JSON input", with nothing to say a cap
// existed. Silently truncating a response is worse than the unbounded read the
// cap was meant to prevent, so this reports.
type limitedBody struct {
	inner  io.ReadCloser
	limit  memory.Size
	read   int64
	capped func()
	fired  bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	remaining := b.limit.Int64() - b.read
	if remaining <= 0 {
		return 0, b.exceeded()
	}

	// Read one byte past the limit, so a body of exactly the limit succeeds
	// and the first byte over is detected rather than inferred.
	if int64(len(p)) > remaining+1 {
		p = p[:remaining+1]
	}

	n, err := b.inner.Read(p)
	b.read += int64(n)

	if b.read > b.limit.Int64() {
		return int(remaining), b.exceeded()
	}
	return n, err
}

func (b *limitedBody) exceeded() error {
	if !b.fired {
		b.fired = true
		if b.capped != nil {
			b.capped()
		}
	}
	return Error.Errorf("response body exceeded %s", b.limit)
}

func (b *limitedBody) Close() error { return b.inner.Close() }

// drain reads a little of a body that is about to be thrown away, then closes
// it.
//
// http.Transport returns a connection to the idle pool only once its body has
// been read to EOF. Closing a 503's body early makes the transport close the
// TCP connection instead - so a service retrying through an incident also
// destroys its own connection pool and adds a fresh TLS handshake to every
// attempt, making the outage worse at the moment it was trying to survive it.
//
// The limit bounds the work, so a multi-megabyte error page is not copied to
// io.Discard for nothing.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}
