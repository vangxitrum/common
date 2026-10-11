package httpsec

import "net/http"

// SwaggerUIPolicy is a Content-Security-Policy that lets Swagger UI, as
// gin-swagger and echo-swagger serve it, render from this server: its
// scripts, stylesheets and images, the inline styles it applies, its
// data: icons, and the fetch of the spec. Nothing loads from another origin
// and nothing may frame the page.
//
// script-src allows inline script because echo-swagger (v1.4.1) starts the UI
// from an inline <script> that embeds the service's own swagger settings, so
// no one hash fits every service. It is acceptable on the docs route alone:
// the page shows the spec and no user data. Never use this policy for a route
// that renders anything a user wrote.
const SwaggerUIPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'"

// ContentSecurityPolicy is one validated Content-Security-Policy, for the
// routes that serve something other than JSON - an API's docs page - and so
// need a looser policy than the one Headers sets on every response.
type ContentSecurityPolicy struct {
	value string
}

// NewContentSecurityPolicy validates policy.
func NewContentSecurityPolicy(policy string) (*ContentSecurityPolicy, error) {
	if policy == "" {
		return nil, Error.Errorf("content security policy is empty")
	}
	if err := headerValue("content security policy", policy); err != nil {
		return nil, err
	}
	return &ContentSecurityPolicy{value: policy}, nil
}

// Apply sets the policy on dst, replacing the one Headers set.
func (p *ContentSecurityPolicy) Apply(dst http.Header) {
	dst.Set("Content-Security-Policy", p.value)
}

// Wrap sets the policy before next runs. Put it inside Headers.Wrap, so it
// replaces the default rather than being replaced by it.
func (p *ContentSecurityPolicy) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Apply(w.Header())
		next.ServeHTTP(w, r)
	})
}
