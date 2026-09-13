package clientip_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"gitlab.internal/tuan.quang.tran/aioz-common/clientip"
)

// A service behind a load balancer in 10.0.0.0/8. The client sent its own
// X-Forwarded-For claiming 1.2.3.4; the load balancer appended the address it
// really saw.
func ExampleResolver_ClientIP() {
	resolver, err := clientip.New(clientip.Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		Header:         "X-Forwarded-For",
	})
	if err != nil {
		panic(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.RemoteAddr = "10.0.0.5:34012" // the load balancer
	req.Header.Set(
		"X-Forwarded-For",
		"1.2.3.4, 203.0.113.9",
	) // client's claim, then the real address

	fmt.Println(resolver.ClientIP(req))
	// Output: 203.0.113.9
}
