package httpsec_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/config"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

func newHeaders(t *testing.T, cfg httpsec.HeadersConfig) *httpsec.Headers {
	t.Helper()
	h, err := httpsec.NewHeaders(cfg)
	require.NoError(t, err)
	return h
}

func serve(handler http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
})

// The OWASP HTTP Headers cheat sheet's set, for an API: nothing may frame,
// sniff, embed or cache a response.
func TestTheDefaultsAreTheOWASPSet(t *testing.T) {
	t.Parallel()

	dst := http.Header{}
	newHeaders(t, httpsec.HeadersConfig{}).Apply(dst)

	require.Equal(t, http.Header{
		"Content-Security-Policy": {
			"default-src 'none'; frame-ancestors 'none'",
		},
		"Cross-Origin-Opener-Policy":   {"same-origin"},
		"Cross-Origin-Resource-Policy": {"same-site"},
		"Permissions-Policy": {
			"geolocation=(), camera=(), microphone=()",
		},
		"Referrer-Policy":           {"strict-origin-when-cross-origin"},
		"Strict-Transport-Security": {"max-age=63072000; includeSubDomains"},
		"X-Content-Type-Options":    {"nosniff"},
		"X-Frame-Options":           {"DENY"},
		"X-Xss-Protection":          {"0"},
		"Cache-Control":             {"no-store"},
	}, dst)
}

func TestWrapSetsTheHeadersOnEveryResponse(t *testing.T) {
	t.Parallel()
	wrapped := newHeaders(
		t,
		httpsec.HeadersConfig{},
	).Wrap(http.NotFoundHandler())

	rec := serve(wrapped, httptest.NewRequest(http.MethodGet, "/missing", nil))

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

// The headers are set before the handler runs, so one endpoint can loosen
// one header - a cacheable public resource - without a second middleware.
func TestAHandlerCanOverrideAHeader(t *testing.T) {
	t.Parallel()
	wrapped := newHeaders(t, httpsec.HeadersConfig{}).Wrap(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=60")
		}),
	)

	rec := serve(wrapped, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
}

func TestHSTSOptions(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		cfg  httpsec.HeadersConfig
		want string
	}{
		"a shorter max-age": {
			httpsec.HeadersConfig{HSTSMaxAge: time.Hour},
			"max-age=3600; includeSubDomains",
		},
		"without subdomains": {
			httpsec.HeadersConfig{HSTSOmitSubdomains: true},
			"max-age=63072000",
		},
		"preload": {
			httpsec.HeadersConfig{HSTSPreload: true},
			"max-age=63072000; includeSubDomains; preload",
		},
	} {
		dst := http.Header{}
		newHeaders(t, c.cfg).Apply(dst)
		require.Equal(t, c.want, dst.Get("Strict-Transport-Security"), name)
	}
}

func TestAllowCachingLeavesCacheControlToTheHandler(t *testing.T) {
	t.Parallel()

	dst := http.Header{}
	newHeaders(t, httpsec.HeadersConfig{AllowCaching: true}).Apply(dst)

	require.Empty(t, dst.Values("Cache-Control"))
}

func TestAnHTMLAppSetsItsOwnPolicy(t *testing.T) {
	t.Parallel()

	dst := http.Header{}
	newHeaders(t, httpsec.HeadersConfig{
		ContentSecurityPolicy:     "default-src 'self'; frame-ancestors 'none'",
		CrossOriginResourcePolicy: "same-origin",
	}).Apply(dst)

	require.Equal(
		t,
		"default-src 'self'; frame-ancestors 'none'",
		dst.Get("Content-Security-Policy"),
	)
	require.Equal(t, "same-origin", dst.Get("Cross-Origin-Resource-Policy"))
}

func TestNewHeadersRejects(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]httpsec.HeadersConfig{
		"a negative max-age":            {HSTSMaxAge: -time.Hour},
		"a max-age under a second":      {HSTSMaxAge: time.Millisecond},
		"preload without subdomains":    {HSTSPreload: true, HSTSOmitSubdomains: true},
		"preload under a year":          {HSTSPreload: true, HSTSMaxAge: 364 * 24 * time.Hour},
		"an unknown resource policy":    {CrossOriginResourcePolicy: "none"},
		"a line break in the policy":    {ContentSecurityPolicy: "default-src 'none'\r\nSet-Cookie: a=b"},
		"a control character in policy": {ContentSecurityPolicy: "default-src\x00 'none'"},
	} {
		_, err := httpsec.NewHeaders(cfg)
		require.Error(t, err, name)
	}
}

func TestConfigBinds(t *testing.T) {
	t.Parallel()

	var cfg struct {
		Headers httpsec.HeadersConfig
		CORS    httpsec.CORSConfig
	}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NotPanics(t, func() { config.BindFlags(flags, &cfg) })

	require.Equal(
		t,
		"default-src 'none'; frame-ancestors 'none'",
		cfg.Headers.ContentSecurityPolicy,
	)
	require.Equal(t, "same-site", cfg.Headers.CrossOriginResourcePolicy)
	require.Equal(t, 2*365*24*time.Hour, cfg.Headers.HSTSMaxAge)
	require.False(t, cfg.Headers.HSTSOmitSubdomains)
	require.False(t, cfg.Headers.HSTSPreload)
	require.False(t, cfg.Headers.AllowCaching)

	require.Empty(t, cfg.CORS.AllowedOrigins)
	require.Equal(
		t,
		[]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"},
		cfg.CORS.AllowedMethods,
	)
	require.Equal(
		t,
		[]string{"Authorization", "Content-Type"},
		cfg.CORS.AllowedHeaders,
	)
	require.Empty(t, cfg.CORS.ExposedHeaders)
	require.False(t, cfg.CORS.AllowCredentials)
	require.Equal(t, 2*time.Hour, cfg.CORS.MaxAge)

	// What was bound is what NewHeaders and NewCORS accept.
	_, err := httpsec.NewHeaders(cfg.Headers)
	require.NoError(t, err)
	_, err = httpsec.NewCORS(cfg.CORS)
	require.NoError(t, err)
}
