package httpapi

import "net/http"

// Option changes how echoapi.Setup and ginapi.Setup install the stack. Each
// one covers an operational choice that differs between services. What a
// client sees - the envelope, the error body, the request id, the security
// headers and CORS - has no option, because a standard every service can
// bend is not a standard.
type Option func(*Options)

// Options is what the Option values passed to Setup add up to. Adapters read
// it; a service only passes Option values.
type Options struct {
	skipLog     map[string]bool
	noAccessLog bool
	noRecovery  bool
	noMetrics   bool
}

// NewOptions applies opts in order.
func NewOptions(opts ...Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// SkipLog leaves requests to these exact URL paths out of the access log
// unless they fail with a server error, so a load balancer's health check
// every few seconds does not bury the requests people read the log for. The
// paths still count in the metrics.
func SkipLog(paths ...string) Option {
	return func(o *Options) {
		if o.skipLog == nil {
			o.skipLog = make(map[string]bool, len(paths))
		}
		for _, p := range paths {
			o.skipLog[p] = true
		}
	}
}

// WithoutAccessLog installs no access log, for a service that writes its own.
// Server faults are still logged with their cause by the error renderer.
func WithoutAccessLog() Option { return func(o *Options) { o.noAccessLog = true } }

// WithoutRecovery installs no panic recovery, for a service that installs its
// own, such as one that reports to an error tracker. That middleware should
// still answer with the standard 500, which returning Recovered(v) as the
// handler's error does.
func WithoutRecovery() Option { return func(o *Options) { o.noRecovery = true } }

// WithoutMetrics records no per-route request metrics, for a service that
// records its own.
func WithoutMetrics() Option { return func(o *Options) { o.noMetrics = true } }

// AccessLog reports whether the access log is installed.
func (o Options) AccessLog() bool { return !o.noAccessLog }

// Recovery reports whether panic recovery is installed.
func (o Options) Recovery() bool { return !o.noRecovery }

// Metrics reports whether per-route metrics are recorded.
func (o Options) Metrics() bool { return !o.noMetrics }

// Logged reports whether a request to path that ended with status goes in the
// access log.
func (o Options) Logged(path string, status int) bool {
	return !o.skipLog[path] || status >= http.StatusInternalServerError
}
