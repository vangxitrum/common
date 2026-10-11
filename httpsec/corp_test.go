package httpsec_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

func TestARouteResourcePolicyReplacesTheDefault(t *testing.T) {
	t.Parallel()

	corp, err := httpsec.NewCrossOriginResourcePolicy("cross-origin")
	require.NoError(t, err)
	media := newHeaders(t, httpsec.HeadersConfig{}).Wrap(
		corp.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})),
	)
	api := newHeaders(t, httpsec.HeadersConfig{}).Wrap(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	)

	rec := serve(media, httptest.NewRequest(http.MethodGet, "/media/1.mp4", nil))
	require.Equal(t, []string{"cross-origin"}, rec.Header().Values("Cross-Origin-Resource-Policy"),
		"one policy, the route's")
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), "the other headers stay")

	rec = serve(api, httptest.NewRequest(http.MethodGet, "/v1/items", nil))
	require.Equal(t, "same-site", rec.Header().Get("Cross-Origin-Resource-Policy"))
}

func TestNewCrossOriginResourcePolicyRejects(t *testing.T) {
	t.Parallel()

	for _, policy := range []string{"", "cross-site", "Cross-Origin", "cross-origin\r\nSet-Cookie: a=b"} {
		_, err := httpsec.NewCrossOriginResourcePolicy(policy)
		require.Error(t, err, policy)
	}
}
