// Package telemetry implements a Prometheus remote_write push client that
// periodically walks a monkit registry and POSTs the collected series to an
// external remote-write receiver (e.g. VictoriaMetrics, Cortex, Mimir, or
// Prometheus's own remote-write receiver). It is a push counterpart to the
// pull-based /metrics endpoint in stats/debug: that endpoint hands out
// since-last-scrape deltas via monkit's DeltaTransformer, while this package
// always reports raw cumulative values, since remote_write receivers expect
// monotonic counters and compute rate()/increase() themselves.
package telemetry

import (
	"strings"
	"time"
)

// Config configures the telemetry push client. It is off by default: leave
// URL empty to disable pushing entirely, matching Storj's off-by-default
// metrics.addr convention.
type Config struct {
	// URL is the remote-write receiver endpoint, e.g.
	// https://vps:9090/api/v1/write. Empty disables the client.
	URL string `help:"Prometheus remote_write receiver URL; empty disables telemetry push" default:""`

	// App identifies this application in every pushed series (label "app").
	App string `help:"app label attached to every pushed series" default:""`
	// Instance identifies this process instance (label "instance"). Empty
	// resolves to the host's hostname (or container id, if determinable)
	// once at client construction, so scaled replicas each get a distinct
	// identity without extra configuration.
	Instance string `help:"instance label attached to every pushed series; empty resolves to hostname at startup" default:""`
	// Role identifies this process's role (label "role"), set per role at
	// deploy time (e.g. "api", "run", "audit").
	Role string `help:"role label attached to every pushed series" default:""`

	// Interval is how often the registry is walked and pushed.
	Interval time.Duration `help:"how often to push metrics to the remote_write receiver" default:"15s"`

	// BearerToken, if set, is sent as an "Authorization: Bearer ..." header.
	// Takes precedence over Username/Password if both are set.
	BearerToken string `user:"true" help:"bearer token for the remote_write receiver" default:""`
	// Username and Password, if set, are sent as HTTP basic auth.
	Username string `user:"true" help:"basic auth username for the remote_write receiver" default:""`
	Password string `user:"true" help:"basic auth password for the remote_write receiver" default:""`

	// IncludePrefixes, if non-empty, restricts pushed series to measurements
	// whose (sanitized) name starts with one of these prefixes. Empty means
	// include every measurement, subject to ExcludePrefixes below.
	IncludePrefixes []string `help:"measurement name prefixes to include (comma-separated); empty includes everything" default:""`
	// ExcludePrefixes drops measurements whose (sanitized) name starts with
	// one of these prefixes. Applied after IncludePrefixes.
	ExcludePrefixes []string `help:"measurement name prefixes to exclude (comma-separated), applied after IncludePrefixes" default:""`
	// DropQuantiles drops monkit's reservoir-sampled quantile fields
	// (r10/r50/r90/r99/rmin/rmax, emitted for every mon.Task/mon.Timer call
	// site) while keeping the exact count/sum/min/max/recent aggregates.
	// These reservoir fields are the largest source of series cardinality in
	// the push path since they're tagged per call site, so dropping them by
	// default keeps a coordinator's series count manageable.
	DropQuantiles bool `help:"drop mon.Task reservoir quantile fields (r10/r50/r90/r99/rmin/rmax); keeps count/sum/min/max" default:"true"`

	// IncludeDefaultGatherer also pushes everything registered with
	// prometheus/client_golang's default registry, for services that
	// instrument with client_golang alongside monkit (the backend's gin HTTP
	// middleware does: http_requests_total, http_request_duration_seconds,
	// http_requests_in_flight). Off by default, since a service that uses
	// only monkit would otherwise pay for the Go collector's series twice -
	// monkit's environment stats already cover the same ground.
	IncludeDefaultGatherer bool `help:"also push metrics from prometheus/client_golang's default registry" default:"false"`
}

// Enabled reports whether the push client should run.
func (cfg Config) Enabled() bool {
	return cfg.URL != ""
}

// quantileFields are the reservoir-sample field names monkit's
// DurationDist/IntDist/FloatDist emit alongside count/sum/min/max/recent
// (see spacemonkeygo/monkit/v3's durdist.go Stats method). They approximate
// percentiles from a fixed-size reservoir; ravg (the reservoir average) is
// kept since, unlike the others, it has no exact counterpart among the
// non-reservoir fields.
var quantileFields = map[string]bool{
	"r10":  true,
	"r50":  true,
	"r90":  true,
	"r99":  true,
	"rmin": true,
	"rmax": true,
}

// dropField reports whether field should be dropped by the cardinality
// filter's quantile rule.
func (cfg Config) dropField(field string) bool {
	return cfg.DropQuantiles && quantileFields[field]
}

// includeMeasurement applies the IncludePrefixes/ExcludePrefixes cardinality
// filter to a sanitized measurement name.
func (cfg Config) includeMeasurement(measurement string) bool {
	if len(cfg.IncludePrefixes) > 0 {
		included := false
		for _, prefix := range cfg.IncludePrefixes {
			if prefix != "" && strings.HasPrefix(measurement, prefix) {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}
	for _, prefix := range cfg.ExcludePrefixes {
		if prefix != "" && strings.HasPrefix(measurement, prefix) {
			return false
		}
	}
	return true
}
