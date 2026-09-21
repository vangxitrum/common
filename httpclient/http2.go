package httpclient

import (
	"net/http"
	"time"
)

// Default health-check timings for an HTTP/2 connection. Conservative enough
// that a healthy idle connection is pinged rarely, tight enough that a dead
// one is retired in seconds rather than at the request deadline.
const (
	DefaultH2HealthCheckInterval = 15 * time.Second
	DefaultH2PingTimeout         = 5 * time.Second
)

// ConfigureH2HealthChecks turns on HTTP/2 connection health checks for
// transport, so a connection that died without a TCP reset is discarded
// instead of being reused.
//
// Go disables this by default - HTTP2Config.SendPingTimeout is zero, which
// means "no health check is performed". A connection broken in a way that
// produces no reset - an expired NAT entry, a middlebox dropping the flow, a
// tunnel restarting - stays in the pool looking healthy, and every request
// multiplexed onto it hangs until its own deadline. For a request/response
// service that is one stalled call; for a push loop it is a blackout lasting
// until the path recovers, because each attempt reuses the same dead
// connection.
//
// interval is how long a connection may go without a frame before a ping, and
// pingTimeout how long to wait for the pong before the connection is closed
// and its requests fail fast. Zero or negative values take the defaults above.
//
// Only connections negotiated as h2 are affected; an HTTP/1.1 connection has
// no multiplexing to stall and is already bounded by ResponseHeaderTimeout.
func ConfigureH2HealthChecks(
	transport *http.Transport,
	interval, pingTimeout time.Duration,
) {
	if interval <= 0 {
		interval = DefaultH2HealthCheckInterval
	}
	if pingTimeout <= 0 {
		pingTimeout = DefaultH2PingTimeout
	}

	if transport.HTTP2 == nil {
		transport.HTTP2 = &http.HTTP2Config{}
	}
	transport.HTTP2.SendPingTimeout = interval
	transport.HTTP2.PingTimeout = pingTimeout
}
