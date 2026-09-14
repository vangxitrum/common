package httpsec

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSConfig says which origins' pages may read responses. The zero value is
// CORS off.
type CORSConfig struct {
	AllowedOrigins   []string      `help:"origins whose pages may read responses: exact (https://app.example.com), subdomains (https://*.example.com), or * for a public API without credentials; empty turns CORS off" default:""`
	AllowedMethods   []string      `help:"methods a preflight allows"                                                                                                                                                   default:"GET,HEAD,POST,PUT,PATCH,DELETE"`
	AllowedHeaders   []string      `help:"request headers a preflight allows"                                                                                                                                           default:"Authorization,Content-Type"`
	ExposedHeaders   []string      `help:"response headers a page may read beyond the CORS-safelisted ones"                                                                                                             default:""`
	AllowCredentials bool          `help:"let pages send cookies and read the responses; never with *"                                                                                                                  default:"false"`
	MaxAge           time.Duration `help:"how long a browser may cache a preflight"                                                                                                                                     default:"2h"`
}

var (
	defaultMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	defaultHeaders = []string{"Authorization", "Content-Type"}
)

const defaultMaxAge = 2 * time.Hour

// CORS answers cross-origin requests for a fixed allowlist.
type CORS struct {
	enabled     bool
	any         bool
	exact       map[string]bool
	wildcards   []wildcard
	credentials bool
	methods     string
	headers     string
	exposed     string
	maxAge      string
}

// wildcard is https://*.example.com: one or more labels, then suffix.
type wildcard struct {
	scheme string
	suffix string // ".example.com"
	port   string // "" for the scheme's default
}

// NewCORS validates config. It refuses the configurations that turn CORS into
// a hole: "*" with credentials, "*" beside other origins, credentials with no
// origins, "null", and anything that is not exactly an origin - a path, a
// trailing slash, user info, a wildcard anywhere but the first label, or one
// that covers a whole top-level domain.
func NewCORS(config CORSConfig) (*CORS, error) {
	if config.AllowedMethods == nil {
		config.AllowedMethods = defaultMethods
	}
	if config.AllowedHeaders == nil {
		config.AllowedHeaders = defaultHeaders
	}
	if config.MaxAge == 0 {
		config.MaxAge = defaultMaxAge
	}

	for _, list := range []struct {
		name   string
		values []string
	}{
		{"method", config.AllowedMethods},
		{"allowed header", config.AllowedHeaders},
		{"exposed header", config.ExposedHeaders},
	} {
		for _, v := range list.values {
			if !isToken(v) {
				return nil, Error.Errorf("%s %q: not a token", list.name, v)
			}
		}
	}
	if config.MaxAge < time.Second {
		return nil, Error.Errorf(
			"max-age %s: want at least a second",
			config.MaxAge,
		)
	}

	c := &CORS{
		exact:       make(map[string]bool),
		credentials: config.AllowCredentials,
		methods:     strings.Join(config.AllowedMethods, ", "),
		headers:     strings.Join(config.AllowedHeaders, ", "),
		exposed:     strings.Join(config.ExposedHeaders, ", "),
		maxAge:      strconv.FormatInt(int64(config.MaxAge/time.Second), 10),
	}

	origins := config.AllowedOrigins
	switch {
	case len(origins) == 0 && config.AllowCredentials:
		return nil, Error.Errorf("credentials allowed with no origins")
	case len(origins) == 0:
		return c, nil
	case len(origins) == 1 && origins[0] == "*":
		if config.AllowCredentials {
			return nil, Error.Errorf(
				"* with credentials would let every website read a logged-in user's data",
			)
		}
		c.enabled, c.any = true, true
		return c, nil
	}

	for _, origin := range origins {
		if origin == "*" {
			return nil, Error.Errorf(
				"* among other origins: use * alone or list the origins",
			)
		}
		scheme, host, port, err := splitOrigin(strings.ToLower(origin))
		if err != nil {
			return nil, Error.Errorf("origin %q: %w", origin, err)
		}
		if suffix, ok := strings.CutPrefix(host, "*"); ok {
			// The rest must be a domain of at least two labels:
			// https://*.com would allow every .com site.
			if !strings.HasPrefix(suffix, ".") || !isHost(suffix[1:]) ||
				!strings.Contains(suffix[1:], ".") {
				return nil, Error.Errorf(
					"origin %q: a wildcard must be the first label of a domain below a TLD",
					origin,
				)
			}
			c.wildcards = append(
				c.wildcards,
				wildcard{scheme: scheme, suffix: suffix, port: port},
			)
			continue
		}
		if !isHost(host) {
			return nil, Error.Errorf("origin %q: invalid host", origin)
		}
		c.exact[join(scheme, host, port)] = true
	}
	c.enabled = true
	return c, nil
}

// Handle sets the CORS response headers for r on dst and reports whether r is
// a preflight, which the caller answers with 204 and no body. With CORS off it
// does nothing.
//
// Vary is added to, never set, so a shared cache does not serve one origin's
// answer to another. The configured method and header lists are sent as they
// are; a request's Access-Control-Request-Headers is never echoed back.
func (c *CORS) Handle(dst http.Header, r *http.Request) (preflight bool) {
	if !c.enabled {
		return false
	}

	preflight = r.Method == http.MethodOptions &&
		r.Header.Get("Access-Control-Request-Method") != ""
	dst.Add("Vary", "Origin")
	if preflight {
		dst.Add("Vary", "Access-Control-Request-Method")
		dst.Add("Vary", "Access-Control-Request-Headers")
	}

	origin, ok := c.allowed(r.Header.Get("Origin"))
	if !ok {
		return preflight
	}
	dst.Set("Access-Control-Allow-Origin", origin)
	if c.credentials {
		dst.Set("Access-Control-Allow-Credentials", "true")
	}
	if preflight {
		dst.Set("Access-Control-Allow-Methods", c.methods)
		if c.headers != "" {
			dst.Set("Access-Control-Allow-Headers", c.headers)
		}
		dst.Set("Access-Control-Max-Age", c.maxAge)
	} else if c.exposed != "" {
		dst.Set("Access-Control-Expose-Headers", c.exposed)
	}
	return preflight
}

// Wrap answers preflights with 204 and passes every other request to next.
// A preflight never reaches next, so a handler does no work for one.
func (c *CORS) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.Handle(w.Header(), r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowed returns the Access-Control-Allow-Origin value for origin. The
// request's Origin is parsed as strictly as the configured ones, so a
// malformed one never matches anything.
func (c *CORS) allowed(origin string) (string, bool) {
	if origin == "" {
		return "", false
	}
	if c.any {
		return "*", true
	}
	scheme, host, port, err := splitOrigin(strings.ToLower(origin))
	if err != nil || !isHost(host) {
		return "", false
	}
	canonical := join(scheme, host, port)
	if c.exact[canonical] {
		return canonical, true
	}
	for _, w := range c.wildcards {
		if w.scheme == scheme && w.port == port &&
			len(host) > len(w.suffix) && strings.HasSuffix(host, w.suffix) {
			return canonical, true
		}
	}
	return "", false
}

// splitOrigin splits a lowercase scheme://host[:port], dropping the scheme's
// default port. The host is returned unchecked; it may hold a wildcard.
func splitOrigin(origin string) (scheme, host, port string, err error) {
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok {
		return "", "", "", Error.Errorf("want scheme://host")
	}
	if scheme != "https" && scheme != "http" {
		return "", "", "", Error.Errorf("scheme %q: want http or https", scheme)
	}
	if rest == "" || strings.ContainsAny(rest, "/?#@\\[]") {
		return "", "", "", Error.Errorf(
			"want only scheme://host[:port], no path, query, user info or IPv6 literal",
		)
	}
	host = rest
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		host, port = rest[:i], rest[i+1:]
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || port[0] == '0' {
			return "", "", "", Error.Errorf("invalid port")
		}
		if scheme == "https" && port == "443" ||
			scheme == "http" && port == "80" {
			port = ""
		}
	}
	return scheme, host, port, nil
}

func join(scheme, host, port string) string {
	if port == "" {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + port
}

// isHost reports whether host is dot-separated labels of a-z, 0-9 and -.
func isHost(host string) bool {
	if host == "" {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// isToken reports whether s is an RFC 9110 token, the grammar of method and
// header names.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
