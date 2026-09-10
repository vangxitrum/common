package httpclient

import (
	"net/http"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("httpclient")

var mon = monkit.Package()

// roundTripperFunc adapts a function to http.RoundTripper, so each layer of
// the chain can be a closure rather than a type.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	return fn(req)
}
