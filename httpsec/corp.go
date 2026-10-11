package httpsec

import "net/http"

// CrossOriginResourcePolicy is one validated Cross-Origin-Resource-Policy, for
// the routes whose responses another site embeds - a <video>, <img> or
// <track> on a customer's page - and so need a looser policy than the
// same-site one Headers sets on every response. A browser refuses a
// cross-site no-cors load of a same-site resource, whatever CORS allows.
type CrossOriginResourcePolicy struct {
	value string
}

// NewCrossOriginResourcePolicy validates policy: same-site, same-origin or
// cross-origin.
func NewCrossOriginResourcePolicy(policy string) (*CrossOriginResourcePolicy, error) {
	switch policy {
	case "same-site", "same-origin", "cross-origin":
	default:
		return nil, Error.Errorf(
			"cross-origin resource policy %q: want same-site, same-origin or cross-origin",
			policy,
		)
	}
	return &CrossOriginResourcePolicy{value: policy}, nil
}

// Apply sets the policy on dst, replacing the one Headers set.
func (p *CrossOriginResourcePolicy) Apply(dst http.Header) {
	dst.Set("Cross-Origin-Resource-Policy", p.value)
}

// Wrap sets the policy before next runs. Put it inside Headers.Wrap, so it
// replaces the default rather than being replaced by it.
func (p *CrossOriginResourcePolicy) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Apply(w.Header())
		next.ServeHTTP(w, r)
	})
}
