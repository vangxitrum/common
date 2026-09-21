package httpclient

import (
	"crypto/tls"
	"net"
	"net/http"
	"runtime"
)

// Transport returns the pooled transport New builds.
//
// It is exported so a service with an unusual requirement - a custom dialer, a
// proxy, client certificates - can start from the configured transport and
// adjust one field rather than abandoning the Config entirely. The result is
// what NewWith takes.
func Transport(config Config) (*http.Transport, error) {
	transport, err := baseTransport(config, false)
	if err != nil {
		return nil, err
	}

	transport.MaxIdleConns = config.MaxIdleConns
	transport.MaxConnsPerHost = config.MaxConnsPerHost

	// Only the pooled profile needs this: OneShotTransport keeps no
	// connections, so it can never reuse a dead one.
	ConfigureH2HealthChecks(
		transport, config.H2HealthCheckInterval, config.H2PingTimeout,
	)

	transport.MaxIdleConnsPerHost = config.MaxIdleConnsPerHost
	if transport.MaxIdleConnsPerHost <= 0 {
		// The standard library keeps two. At any real concurrency that means
		// opening and discarding a connection - and a TLS handshake - per
		// request, which turns a 5ms dependency into a 50ms one.
		transport.MaxIdleConnsPerHost = runtime.GOMAXPROCS(0) + 1
	}

	return transport, nil
}

// OneShotTransport returns the transport NewOneShot builds: no keep-alives and
// no idle pool.
//
// Reusing a pooled transport for an address you will not see again leaks file
// descriptors, because every host it ever spoke to keeps idle connections
// until they time out. This one holds nothing.
func OneShotTransport(config Config) (*http.Transport, error) {
	transport, err := baseTransport(config, true)
	if err != nil {
		return nil, err
	}

	transport.DisableKeepAlives = true
	transport.MaxIdleConnsPerHost = -1

	return transport, nil
}

// baseTransport is what both profiles share. blockByDefault is the profile's
// own answer to an empty PrivateNetworks.
func baseTransport(config Config, blockByDefault bool) (*http.Transport, error) {
	tlsConfig, err := config.tlsConfig()
	if err != nil {
		return nil, err
	}

	guard, err := config.guard(blockByDefault)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout:   config.DialTimeout,
		KeepAlive: config.KeepAlive,
	}

	proxy := http.ProxyFromEnvironment
	if guard != nil {
		dialer.Control = guard.control
		// Through a proxy the guard would only ever see the proxy's address,
		// and the proxy would connect wherever the URL says.
		proxy = nil
	}

	return &http.Transport{
		Proxy:                 proxy,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   config.TLSHandshakeTimeout,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
		ExpectContinueTimeout: config.ExpectContinueTimeout,
		IdleConnTimeout:       config.IdleConnTimeout,
		DisableCompression:    config.DisableCompression,
		ForceAttemptHTTP2:     config.ForceHTTP2,
	}, nil
}

// tlsConfig translates the two TLS settings, and rejects anything else.
func (config Config) tlsConfig() (*tls.Config, error) {
	var minVersion uint16
	switch config.TLSMinVersion {
	case "", "1.2":
		minVersion = tls.VersionTLS12
	case "1.3":
		minVersion = tls.VersionTLS13
	default:
		// TLS 1.0 and 1.1 are deprecated and are not offered as an option: a
		// shared library that lets a flag re-enable them is a shared library
		// that will have them enabled somewhere.
		return nil, Error.Errorf(
			"tls-min-version %q is not 1.2 or 1.3",
			config.TLSMinVersion,
		)
	}

	return &tls.Config{
		MinVersion:         minVersion,
		InsecureSkipVerify: config.TLSSkipVerify, //nolint:gosec // guarded by a hidden, test-only flag
	}, nil
}

// idempotent reports whether replaying the request can be assumed safe.
//
// The methods are the ones RFC 9110 defines as idempotent. POST is not among
// them, and that omission is the point: a POST that timed out may well have
// succeeded, so replaying it charges the card twice.
func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}
