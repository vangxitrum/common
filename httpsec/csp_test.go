package httpsec_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

func TestARoutePolicyReplacesTheDefault(t *testing.T) {
	t.Parallel()

	csp, err := httpsec.NewContentSecurityPolicy(httpsec.SwaggerUIPolicy)
	require.NoError(t, err)
	docs := newHeaders(t, httpsec.HeadersConfig{}).Wrap(
		csp.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})),
	)
	api := newHeaders(t, httpsec.HeadersConfig{}).Wrap(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	)

	rec := serve(docs, httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil))
	require.Equal(t, []string{httpsec.SwaggerUIPolicy}, rec.Header().Values("Content-Security-Policy"),
		"one policy, the route's: two would both apply, and the stricter wins")
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), "the other headers stay")

	rec = serve(api, httptest.NewRequest(http.MethodGet, "/v1/items", nil))
	require.Equal(t, "default-src 'none'; frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"))
}

func TestNewContentSecurityPolicyRejects(t *testing.T) {
	t.Parallel()

	for name, policy := range map[string]string{
		"an empty policy":     "",
		"a line break":        "default-src 'self'\r\nSet-Cookie: a=b",
		"a control character": "default-src\x00 'self'",
	} {
		_, err := httpsec.NewContentSecurityPolicy(policy)
		require.Error(t, err, name)
	}
}
