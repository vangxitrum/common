package httpsec

import (
	"net/http"
	"strconv"
	"time"
)

// HeadersConfig adjusts the security headers. The zero value is the defaults,
// which suit a JSON API: nothing may frame, sniff, embed or cache a response.
type HeadersConfig struct {
	ContentSecurityPolicy     string        `help:"Content-Security-Policy; the default suits an API that serves no HTML"                       default:"default-src 'none'; frame-ancestors 'none'"`
	CrossOriginResourcePolicy string        `help:"Cross-Origin-Resource-Policy: same-site, same-origin or cross-origin"                        default:"same-site"`
	HSTS                      bool          `help:"send Strict-Transport-Security; turn on once this host and every host under it serve HTTPS only" default:"false"`
	HSTSMaxAge                time.Duration `help:"Strict-Transport-Security max-age"                                                           default:"17520h"`
	HSTSOmitSubdomains        bool          `help:"leave includeSubDomains out of Strict-Transport-Security"                                    default:"false"`
	HSTSPreload               bool          `help:"add preload to Strict-Transport-Security; needs subdomains and a max-age of at least a year" default:"false"`
	AllowCaching              bool          `help:"leave Cache-Control to the handler instead of sending no-store"                              default:"false"`
}

const (
	defaultCSP        = "default-src 'none'; frame-ancestors 'none'"
	defaultCORP       = "same-site"
	defaultHSTSMaxAge = 2 * 365 * 24 * time.Hour
	preloadMinMaxAge  = 365 * 24 * time.Hour
)

// Headers is a fixed set of response headers, computed once.
type Headers struct {
	set [][2]string
}

// NewHeaders validates config and computes the header set.
//
// Strict-Transport-Security is sent only when HSTS is on. It is off by
// default for now: a browser that sees it refuses plain HTTP to this host,
// and with includeSubDomains to every host under it, for the whole max-age -
// two years by default - and nothing the server says later shortens that for
// a browser that already has it. Turn it on per service once every host it
// covers serves HTTPS only. The HSTS settings are validated either way, so a
// bad value is found before it is switched on.
func NewHeaders(config HeadersConfig) (*Headers, error) {
	if config.ContentSecurityPolicy == "" {
		config.ContentSecurityPolicy = defaultCSP
	}
	if config.CrossOriginResourcePolicy == "" {
		config.CrossOriginResourcePolicy = defaultCORP
	}
	if config.HSTSMaxAge == 0 {
		config.HSTSMaxAge = defaultHSTSMaxAge
	}

	if err := headerValue(
		"content security policy",
		config.ContentSecurityPolicy,
	); err != nil {
		return nil, err
	}
	switch config.CrossOriginResourcePolicy {
	case "same-site", "same-origin", "cross-origin":
	default:
		return nil, Error.Errorf(
			"cross-origin resource policy %q: want same-site, same-origin or cross-origin",
			config.CrossOriginResourcePolicy,
		)
	}
	if config.HSTSMaxAge < time.Second {
		return nil, Error.Errorf(
			"hsts max-age %s: want at least a second",
			config.HSTSMaxAge,
		)
	}
	if config.HSTSPreload && !config.HSTS {
		return nil, Error.Errorf("hsts preload is set but hsts is off")
	}
	if config.HSTSPreload &&
		(config.HSTSOmitSubdomains || config.HSTSMaxAge < preloadMinMaxAge) {
		return nil, Error.Errorf(
			"hsts preload needs includeSubDomains and a max-age of at least a year",
		)
	}

	hsts := "max-age=" + strconv.FormatInt(
		int64(config.HSTSMaxAge/time.Second),
		10,
	)
	if !config.HSTSOmitSubdomains {
		hsts += "; includeSubDomains"
	}
	if config.HSTSPreload {
		hsts += "; preload"
	}

	h := &Headers{set: [][2]string{
		{"Content-Security-Policy", config.ContentSecurityPolicy},
		{"Cross-Origin-Opener-Policy", "same-origin"},
		{"Cross-Origin-Resource-Policy", config.CrossOriginResourcePolicy},
		{"Permissions-Policy", "geolocation=(), camera=(), microphone=()"},
		{"Referrer-Policy", "strict-origin-when-cross-origin"},
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		// 0, not 1: the old XSS auditors it controlled could be abused to
		// leak data, and no current browser has one.
		{"X-Xss-Protection", "0"},
	}}
	if config.HSTS {
		h.set = append(h.set, [2]string{"Strict-Transport-Security", hsts})
	}
	if !config.AllowCaching {
		h.set = append(h.set, [2]string{"Cache-Control", "no-store"})
	}
	return h, nil
}

// Apply sets the headers on dst, replacing any values already there.
func (h *Headers) Apply(dst http.Header) {
	for _, kv := range h.set {
		dst.Set(kv[0], kv[1])
	}
}

// Wrap sets the headers before next runs, so a handler can still override
// one - a Cache-Control on a public, cacheable resource.
func (h *Headers) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Apply(w.Header())
		next.ServeHTTP(w, r)
	})
}

// headerValue refuses control characters, which would split or corrupt the
// header they are written into.
func headerValue(name, value string) error {
	for i := range len(value) {
		if c := value[i]; c < 0x20 || c == 0x7f {
			return Error.Errorf("%s: control character at byte %d", name, i)
		}
	}
	return nil
}
