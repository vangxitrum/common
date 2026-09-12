package httpclient

import (
	"errors"
	"net/http"

	"github.com/spacemonkeygo/monkit/v3"
)

// errRedirect is what a refused redirect wraps, so a caller can tell one from a
// transport failure and DefaultRetryable knows not to try again.
var errRedirect = errors.New("redirect refused")

// checkRedirect builds the policy http.Client applies between hops.
//
// What the standard library already does, so this package does not claim it:
// with no policy it stops after ten redirects, and since Go 1.8 it strips
// Authorization, WWW-Authenticate and Cookie when a redirect crosses to another
// domain. This adds exactly two things - a configurable cap, including refusing
// to follow at all - and a refusal to be downgraded from https to http.
//
// Redirects are handled by http.Client, above the whole RoundTripper chain, so
// a redirect chain produces one metrics sample per hop, and each attempt
// Retry.Do makes is a new Client.Do with a redirect count of its own. Both are
// what you want and both surprise people.
func (config Config) checkRedirect() func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if config.MaxRedirects == 0 {
			// Not an error: a caller that wants the Location header wants the
			// 3xx response, and making zero mean "fail on any redirect" would
			// leave no way to say "do not follow".
			return http.ErrUseLastResponse
		}

		if !config.AllowDowngrade && len(via) > 0 &&
			via[0].URL.Scheme == "https" && req.URL.Scheme == "http" {
			// The one thing Go does not refuse on its own. Following it sends
			// whatever the request carries over the wire in clear text.
			mon.Event(
				"http_redirect_refused",
				monkit.NewSeriesTag("reason", "downgrade"),
			)
			return Error.Errorf(
				"refusing to follow an https redirect to http at %s: %w",
				req.URL.Host,
				errRedirect,
			)
		}

		// via holds the requests already made, so the first redirect sees one
		// entry. Greater-than, not greater-or-equal, is what makes
		// MaxRedirects the number of redirects followed - the same meaning the
		// standard library's own limit of ten has.
		if len(via) > config.MaxRedirects {
			mon.Event(
				"http_redirect_refused",
				monkit.NewSeriesTag("reason", "limit"),
			)
			return Error.Errorf("stopped after %d redirects, last to %s: %w",
				config.MaxRedirects, req.URL.Host, errRedirect)
		}

		return nil
	}
}
