package httpsec_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

func newCORS(t *testing.T, cfg httpsec.CORSConfig) *httpsec.CORS {
	t.Helper()
	c, err := httpsec.NewCORS(cfg)
	require.NoError(t, err)
	return c
}

func get(origin string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/things", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func preflight(origin string, requestHeaders ...string) *http.Request {
	r := httptest.NewRequest(http.MethodOptions, "/v1/things", nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("Access-Control-Request-Method", http.MethodPut)
	if len(requestHeaders) > 0 {
		r.Header.Set(
			"Access-Control-Request-Headers",
			strings.Join(requestHeaders, ", "),
		)
	}
	return r
}

// counting is a handler that records whether it ran.
func counting() (http.Handler, *atomic.Int64) {
	var calls atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("ok"))
	}), &calls
}

const app = "https://app.example.com"

// Most of our CORS configs allow any origin because a library defaulted to
// it. Here the default is off: no origins, no CORS headers at all.
func TestAnEmptyAllowlistDisablesCORS(t *testing.T) {
	t.Parallel()
	next, calls := counting()
	wrapped := newCORS(t, httpsec.CORSConfig{}).Wrap(next)

	for _, r := range []*http.Request{get(app), preflight(app)} {
		rec := serve(wrapped, r)
		require.Empty(
			t,
			rec.Header().Values("Access-Control-Allow-Origin"),
			r.Method,
		)
		require.Empty(t, rec.Header().Values("Vary"), r.Method)
	}
	require.Equal(t, int64(2), calls.Load(), "both went to the handler")
}

func TestAnAllowedOriginIsEchoed(t *testing.T) {
	t.Parallel()
	next, calls := counting()
	wrapped := newCORS(t, httpsec.CORSConfig{
		AllowedOrigins: []string{app},
		ExposedHeaders: []string{"X-Request-Id"},
	}).Wrap(next)

	rec := serve(wrapped, get(app))

	require.Equal(t, app, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(
		t,
		"X-Request-Id",
		rec.Header().Get("Access-Control-Expose-Headers"),
	)
	require.Empty(t, rec.Header().Values("Access-Control-Allow-Credentials"))
	require.Contains(t, rec.Header().Values("Vary"), "Origin")
	require.Equal(t, int64(1), calls.Load())
}

// CORS decides what a browser lets a page read, not who may call: a refused
// origin is still served, just without the header that would let the page
// see the answer. Authorization is the handler's job.
func TestARefusedOriginIsServedWithoutAllowOrigin(t *testing.T) {
	t.Parallel()
	next, calls := counting()
	wrapped := newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{app}},
	).Wrap(next)

	rec := serve(wrapped, get("https://evil.example.net"))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Values("Access-Control-Allow-Origin"))
	require.Contains(
		t,
		rec.Header().Values("Vary"),
		"Origin",
		"a cache must not serve this to an allowed origin",
	)
	require.Equal(t, int64(1), calls.Load())
}

// A response that differs by Origin must say so, or a shared cache hands one
// origin's answer to another. Vary is added to, never replaced.
func TestVaryIsAddedToNotReplaced(t *testing.T) {
	t.Parallel()

	dst := http.Header{"Vary": {"Accept-Encoding"}}
	newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{app}},
	).Handle(dst, get(app))

	require.Equal(t, []string{"Accept-Encoding", "Origin"}, dst.Values("Vary"))
}

func TestAPreflightFromAnAllowedOrigin(t *testing.T) {
	t.Parallel()
	next, calls := counting()
	wrapped := newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{app}},
	).Wrap(next)

	rec := serve(wrapped, preflight(app, "Content-Type"))

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, app, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(
		t,
		"GET, HEAD, POST, PUT, PATCH, DELETE",
		rec.Header().Get("Access-Control-Allow-Methods"),
	)
	require.Equal(
		t,
		"Authorization, Content-Type",
		rec.Header().Get("Access-Control-Allow-Headers"),
	)
	require.Equal(t, "7200", rec.Header().Get("Access-Control-Max-Age"))
	require.Subset(
		t,
		rec.Header().Values("Vary"),
		[]string{
			"Origin",
			"Access-Control-Request-Method",
			"Access-Control-Request-Headers",
		},
	)
	require.Zero(t, calls.Load(), "a preflight never reaches the handler")
}

// A preflight from a refused origin is answered too - with nothing a browser
// would accept - so it never reaches a handler that might do work for it.
func TestAPreflightFromARefusedOrigin(t *testing.T) {
	t.Parallel()
	next, calls := counting()
	wrapped := newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{app}},
	).Wrap(next)

	rec := serve(wrapped, preflight("https://evil.example.net"))

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Header().Values("Access-Control-Allow-Origin"))
	require.Empty(t, rec.Header().Values("Access-Control-Allow-Methods"))
	require.Zero(t, calls.Load())
}

// The configured lists are sent, never the request's echoed back: echoing
// Access-Control-Request-Headers allows whatever a page asks for.
func TestRequestedHeadersAreNeverEchoed(t *testing.T) {
	t.Parallel()
	wrapped := newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{app}},
	).Wrap(ok)

	rec := serve(wrapped, preflight(app, "X-Evil", "Authorization"))

	require.Equal(
		t,
		"Authorization, Content-Type",
		rec.Header().Get("Access-Control-Allow-Headers"),
	)
}

// An OPTIONS request without Access-Control-Request-Method is not a
// preflight; it belongs to the handler.
func TestAPlainOptionsRequestReachesTheHandler(t *testing.T) {
	t.Parallel()
	next, calls := counting()
	wrapped := newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{app}},
	).Wrap(next)

	r := httptest.NewRequest(http.MethodOptions, "/v1/things", nil)
	r.Header.Set("Origin", app)
	serve(wrapped, r)

	require.Equal(t, int64(1), calls.Load())
}

func TestCredentialsAreAllowedOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	dst := http.Header{}
	newCORS(
		t,
		httpsec.CORSConfig{
			AllowedOrigins:   []string{app},
			AllowCredentials: true,
		},
	).Handle(dst, get(app))

	require.Equal(t, "true", dst.Get("Access-Control-Allow-Credentials"))
	require.Equal(t, app, dst.Get("Access-Control-Allow-Origin"))
}

// "*" is for a public, credential-free API, and then it is sent as "*".
func TestAStarAllowsAnyOriginWithoutCredentials(t *testing.T) {
	t.Parallel()

	dst := http.Header{}
	newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{"*"}},
	).Handle(dst, get("https://anyone.example.org"))

	require.Equal(t, "*", dst.Get("Access-Control-Allow-Origin"))
	require.Empty(t, dst.Values("Access-Control-Allow-Credentials"))
}

// A wildcard stands for one or more subdomain labels: never the apex, never
// a domain that merely ends in the same letters, never another scheme or port.
func TestAWildcardMatchesSubdomainsOnly(t *testing.T) {
	t.Parallel()
	c := newCORS(
		t,
		httpsec.CORSConfig{AllowedOrigins: []string{"https://*.example.com"}},
	)

	for origin, allowed := range map[string]bool{
		"https://app.example.com":         true,
		"https://a.b.example.com":         true,
		"https://APP.example.com":         true,
		"https://example.com":             false,
		"https://evilexample.com":         false,
		"https://.example.com":            false,
		"https://app.example.com.evil.io": false,
		"http://app.example.com":          false,
		"https://app.example.com:8443":    false,
		"https://app.example.com/":        false,
		"https://ev il.example.com":       false,
		"https://user@app.example.com":    false,
		"null":                            false,
	} {
		dst := http.Header{}
		c.Handle(dst, get(origin))
		if allowed {
			require.Equal(
				t,
				strings.ToLower(origin),
				dst.Get("Access-Control-Allow-Origin"),
				origin,
			)
		} else {
			require.Empty(t, dst.Values("Access-Control-Allow-Origin"), origin)
		}
	}
}

// Browsers leave the default port out of Origin, so a configured :443 must
// still match.
func TestAConfiguredDefaultPortIsNormalized(t *testing.T) {
	t.Parallel()
	c := newCORS(
		t,
		httpsec.CORSConfig{
			AllowedOrigins: []string{
				"HTTPS://App.Example.com:443",
				"http://localhost:3000",
			},
		},
	)

	for _, origin := range []string{app, "http://localhost:3000"} {
		dst := http.Header{}
		c.Handle(dst, get(origin))
		require.Equal(t, origin, dst.Get("Access-Control-Allow-Origin"), origin)
	}
}

func TestNewCORSRejects(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]httpsec.CORSConfig{
		"a star with credentials":     {AllowedOrigins: []string{"*"}, AllowCredentials: true},
		"a star among other origins":  {AllowedOrigins: []string{"*", app}},
		"credentials with no origins": {AllowCredentials: true},
		"null":                        {AllowedOrigins: []string{"null"}},
		"a path":                      {AllowedOrigins: []string{app + "/login"}},
		"a trailing slash":            {AllowedOrigins: []string{app + "/"}},
		"a query":                     {AllowedOrigins: []string{app + "?a=b"}},
		"no scheme":                   {AllowedOrigins: []string{"app.example.com"}},
		"another scheme":              {AllowedOrigins: []string{"ftp://app.example.com"}},
		"user info":                   {AllowedOrigins: []string{"https://user@app.example.com"}},
		"a wildcard over a TLD":       {AllowedOrigins: []string{"https://*.com"}},
		"a wildcard alone":            {AllowedOrigins: []string{"https://*"}},
		"a wildcard mid-host":         {AllowedOrigins: []string{"https://app.*.example.com"}},
		"a partial wildcard":          {AllowedOrigins: []string{"https://app*.example.com"}},
		"an empty origin":             {AllowedOrigins: []string{""}},
		"a method with a space":       {AllowedOrigins: []string{app}, AllowedMethods: []string{"GET POST"}},
		"a header with a colon":       {AllowedOrigins: []string{app}, AllowedHeaders: []string{"X-A: b"}},
		"a negative max-age":          {AllowedOrigins: []string{app}, MaxAge: -time.Second},
	} {
		_, err := httpsec.NewCORS(cfg)
		require.Error(t, err, name)
	}
}
