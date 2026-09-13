// Package httpsec sets the response headers OWASP's HTTP Headers, HSTS and
// CORS cheat sheets ask of every service, for any router.
//
// A survey of our services found security headers in two of ten repositories,
// HSTS in none, and eight of eleven CORS configs allowing any origin - one of
// them reflecting any Origin with credentials, which lets every website on the
// internet read a logged-in user's data. Each service had written, or not
// written, its own middleware for its own framework.
//
// This package is framework-free: Headers.Apply and CORS.Handle work on an
// http.Header and an *http.Request, and Wrap is net/http's own middleware
// shape. A gin or echo service calls them from a few lines of its own
// middleware; the README has both.
//
// # What this package does not do
//
// CORS is not authorization. It decides which pages a browser lets read a
// response, not who may call: a refused origin is still served. For
// credentialed CORS pair it with Go's http.CrossOriginProtection, which
// refuses cross-origin state-changing requests (CSRF) outright.
package httpsec

import "github.com/zeebo/errs/v2"

// Error tags every failure this package produces.
var Error = errs.Tag("httpsec")
