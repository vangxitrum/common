// Package httpclient constructs a configured, observable outbound HTTP client.
//
// http.DefaultClient has no timeout. A dependency that accepts a connection and
// then never answers holds the calling goroutine, its connection and its
// request for as long as the process lives, so a service that calls one on a
// request path dies of a fault in something it merely talks to. That is the
// failure this package exists for.
//
// The second one is quieter. http.DefaultTransport keeps two idle connections
// per host, so at any real concurrency a service opens and discards a
// connection - and pays a TLS handshake - on nearly every request, which turns
// a five millisecond dependency into a fifty millisecond one and shows up as
// "the network is slow" rather than as a setting nobody chose.
//
// # A client, and deliberately nothing else
//
// This package is pure net/http: it returns an *http.Client and composes
// http.RoundTripper. There is no server, no router and no middleware here, and
// there will not be - services are not required to share an HTTP framework, so
// anything tied to one would be unusable by half of them. What they do share is
// the outbound side, because *http.Client is the type every framework and every
// vendored SDK already accepts.
//
// # The client never retries
//
// A client that retries on its own triples the load on a dependency that is
// already failing, and a replayed POST that timed out after succeeding charges
// the card twice. So every call is one attempt, and retrying is something the
// caller does, in the open, with [Retry.Do]:
//
//	policy := httpclient.Retry{Name: "billing", MaxAttempts: 4}
//	resp, err := policy.Do(ctx, client, newRequest)
package httpclient

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
	"gitlab.internal/tuan.quang.tran/aioz-common/version"
)

// Config describes one outbound client.
//
// The defaults follow the pooled profile that hashicorp's go-cleanhttp
// established, because they are the ones a decade of production has sanded
// down, not because they are magic.
type Config struct {
	Name string `help:"client name; tags this client's metrics so two clients can be told apart" default:"http"`

	Timeout time.Duration `help:"backstop on a whole request including reading the body; a context deadline is the real bound; 0 disables" default:"30s"`

	DialTimeout           time.Duration `help:"timeout establishing a tcp connection"                              default:"30s"`
	KeepAlive             time.Duration `help:"tcp keepalive probe interval; 0 uses the system default"            default:"30s"`
	TLSHandshakeTimeout   time.Duration `help:"timeout for the tls handshake"                                      default:"10s"`
	ResponseHeaderTimeout time.Duration `help:"wait for response headers after the request is written; 0 disables" default:"0"`
	ExpectContinueTimeout time.Duration `help:"wait for a 100-continue after sending an Expect header"             default:"1s"`
	IdleConnTimeout       time.Duration `help:"how long an idle pooled connection is kept"                         default:"90s"`

	MaxIdleConns        int `help:"idle connections kept across all hosts; 0 means unlimited"        default:"100"`
	MaxIdleConnsPerHost int `help:"idle connections kept per host; 0 uses one per CPU plus one"      default:"0"`
	MaxConnsPerHost     int `help:"connections per host including in-flight ones; 0 means unlimited" default:"0"`

	DisableCompression bool `help:"do not ask for gzip; only for a caller that decompresses itself" default:"false"`
	ForceHTTP2         bool `help:"offer http/2 in the tls handshake"                               default:"true"`

	TLSMinVersion string `help:"minimum tls version, 1.2 or 1.3"                     default:"1.2"`
	TLSSkipVerify bool   `help:"do not verify the server certificate; test use only" default:"false" hidden:"true"`

	MaxRedirects   int  `help:"redirects followed before the request fails; 0 returns the redirect response instead of following it" default:"10"`
	AllowDowngrade bool `help:"follow an https redirect to http; sends credentials and bodies in clear text"                         default:"false" hidden:"true"`

	MaxResponseSize memory.Size `help:"cap on a response body read through this client; 0 disables the cap" default:"32MiB"`

	PrivateNetworks string   `help:"connections to loopback, private, link-local and other internal addresses: allow or block; empty lets the constructor choose - New allows, NewOneShot blocks" default:""`
	AllowedNetworks []string `help:"address prefixes exempt from blocking, such as 10.20.0.0/16"                                                                                              default:""`

	UserAgent string `help:"user-agent header; empty derives one from the binary name and the build stamp" default:""`

	Metrics bool `help:"record monkit series for requests"                                                        default:"true"`
	HostTag bool `help:"add a host tag to the request counter; only for a client with a small fixed set of hosts" default:"false" hidden:"true"`
}

// Validate reports configuration that binds and parses but cannot work.
func (config Config) Validate() error {
	if _, err := config.tlsConfig(); err != nil {
		return err
	}

	for name, d := range map[string]time.Duration{
		"timeout":                 config.Timeout,
		"dial-timeout":            config.DialTimeout,
		"tls-handshake-timeout":   config.TLSHandshakeTimeout,
		"response-header-timeout": config.ResponseHeaderTimeout,
		"idle-conn-timeout":       config.IdleConnTimeout,
	} {
		if d < 0 {
			return Error.Errorf("%s must not be negative", name)
		}
	}

	if config.MaxRedirects < 0 {
		return Error.Errorf("max-redirects must not be negative")
	}
	if config.MaxResponseSize < 0 {
		return Error.Errorf("max-response-size must not be negative")
	}
	if _, err := config.guard(false); err != nil {
		return err
	}
	return nil
}

// New returns a client to keep for the life of the process.
//
// This is the one to reach for. Its transport pools connections, so a
// dependency called repeatedly is called over a connection that is already
// open, which is the difference this package makes most often.
//
// The returned client is an ordinary *http.Client: hand it to any SDK that
// takes one. There is no Chain call to make, because an http.Transport exports
// no pool counters to poll - every number this package has is pushed at the
// moment it happens.
func New(log *slog.Logger, config Config) (*http.Client, error) {
	transport, err := Transport(config)
	if err != nil {
		return nil, err
	}
	return NewWith(log, config, transport)
}

// NewOneShot returns a client for a request or two to an address you will not
// see again.
//
// Keep-alives are off and nothing is pooled, because a pooled transport used
// for transient destinations keeps an idle connection per host it ever spoke to
// and leaks file descriptors until they time out. Reach for this only when the
// address is genuinely one-off; a client per dependency, built with New, is
// what almost every call site wants.
//
// It also refuses to connect to internal addresses - loopback, private
// networks, link-local and the cloud metadata endpoint among them - because an
// address you will not see again usually came from someone else, and that is
// how server-side request forgery reaches inside. PrivateNetworks and
// AllowedNetworks change that; a refusal wraps ErrBlockedAddress.
func NewOneShot(log *slog.Logger, config Config) (*http.Client, error) {
	transport, err := OneShotTransport(config)
	if err != nil {
		return nil, err
	}
	return NewWith(log, config, transport)
}

// NewWith is New over a transport the caller built - client certificates, a
// custom dialer, a tracing wrapper.
//
// The chain, the timeout and the redirect policy are applied on top of it, so
// an unusual transport does not mean giving up everything else.
func NewWith(
	log *slog.Logger,
	config Config,
	base http.RoundTripper,
) (*http.Client, error) {
	if log == nil {
		return nil, Error.Errorf("logger is nil")
	}
	if base == nil {
		return nil, Error.Errorf("transport is nil")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}

	name := config.clientName()
	nameTag := monkit.NewSeriesTag("name", name)
	config.Name = name

	m := newMetrics(nameTag, config.HostTag)

	// Innermost first. The body cap is innermost, the only position where
	// nothing above it can have replaced the body already. There is no retry
	// layer: each call is one attempt, and Retry.Do makes one call per attempt,
	// so a metrics sample is always one attempt on the wire.
	chain := bodyLimitTransport(config.MaxResponseSize, func() {
		m.capped.Inc(1)
	}, base)

	if config.Metrics {
		chain = m.transport(chain)
	}

	chain = requestIDTransport(chain)
	chain = userAgentTransport(config.userAgent(), chain)

	return &http.Client{
		Transport:     chain,
		Timeout:       config.Timeout,
		CheckRedirect: config.checkRedirect(),
	}, nil
}

// clientName is the name this client's series are tagged with.
func (config Config) clientName() string {
	if config.Name == "" {
		return "http"
	}
	return config.Name
}

// userAgent is what this client identifies itself as.
func (config Config) userAgent() string {
	if config.UserAgent != "" {
		return config.UserAgent
	}
	// os.Args[0] is a temporary path under `go test`, which is harmless: the
	// config field is the override for anyone who cares what the remote sees.
	return version.Build.UserAgent(filepath.Base(os.Args[0]))
}
