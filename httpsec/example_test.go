package httpsec_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

func Example() {
	headers, err := httpsec.NewHeaders(httpsec.HeadersConfig{})
	if err != nil {
		panic(err)
	}
	cors, err := httpsec.NewCORS(httpsec.CORSConfig{
		AllowedOrigins: []string{"https://*.example.com"},
	})
	if err != nil {
		panic(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	handler := headers.Wrap(cors.Wrap(api))

	for _, origin := range []string{"https://app.example.com", "https://evil.example.net"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/things", nil)
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)

		fmt.Printf("%s: %d, allow-origin %q, %s\n", origin, rec.Code,
			rec.Header().Get("Access-Control-Allow-Origin"),
			rec.Header().Get("X-Content-Type-Options"))
	}
	// Output:
	// https://app.example.com: 200, allow-origin "https://app.example.com", nosniff
	// https://evil.example.net: 200, allow-origin "", nosniff
}
