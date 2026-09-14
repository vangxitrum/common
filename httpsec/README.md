# httpsec

The response headers OWASP's HTTP Headers, HSTS and CORS cheat sheets ask of
every service, for any router.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
```

A survey of our services (2026-09-12) found security headers in two of ten
repositories, HSTS in none, and eight of eleven CORS configs allowing any
origin - one reflecting any `Origin` with credentials, which lets every website
read a logged-in user's data. This package is one answer for all of them. It
imports no framework: gin and echo call it from a few lines each, below.

## Usage

```go
headers, err := httpsec.NewHeaders(cfg.Headers)
if err != nil {
    return err
}
cors, err := httpsec.NewCORS(cfg.CORS)
if err != nil {
    return err
}

handler = headers.Wrap(cors.Wrap(mux)) // net/http
```

```yaml
headers:
  content-security-policy: "default-src 'none'; frame-ancestors 'none'"
  cross-origin-resource-policy: same-site
  hsts-max-age: 17520h
cors:
  allowed-origins: [https://app.aioz.io, https://*.aioz.io]  # empty: CORS off
  allow-credentials: true
```

### gin

```go
router.Use(func(c *gin.Context) {
    headers.Apply(c.Writer.Header())
    if cors.Handle(c.Writer.Header(), c.Request) {
        c.AbortWithStatus(http.StatusNoContent) // a preflight
        return
    }
    c.Next()
})
```

Global middleware also runs for unmatched routes, so a preflight to a path
with no `OPTIONS` route is still answered.

### echo

```go
e.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
    return func(c echo.Context) error {
        headers.Apply(c.Response().Header())
        if cors.Handle(c.Response().Header(), c.Request()) {
            return c.NoContent(http.StatusNoContent) // a preflight
        }
        return next(c)
    }
})
```

`e.Pre` runs before routing, so echo's own `OPTIONS` handling never sees a
preflight.

In both, remove the framework's CORS middleware (`gin-contrib/cors`,
`middleware.CORS()`): two of them answer twice, with different rules.

## Headers

| Header | Value | Why |
| --- | --- | --- |
| `Content-Security-Policy` | `default-src 'none'; frame-ancestors 'none'` | An API serves no HTML; a response rendered as a page can run nothing |
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains` | Browsers refuse plain HTTP for two years |
| `X-Content-Type-Options` | `nosniff` | JSON is never sniffed into HTML or script |
| `X-Frame-Options` | `DENY` | No clickjacking (older browsers; CSP covers the rest) |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | URLs, with their tokens, stay on our site |
| `Cross-Origin-Opener-Policy` | `same-origin` | No cross-origin window handle |
| `Cross-Origin-Resource-Policy` | `same-site` | Other sites cannot embed responses |
| `Permissions-Policy` | `geolocation=(), camera=(), microphone=()` | Nothing may ask for them |
| `X-XSS-Protection` | `0` | The old auditor could leak data; none remain |
| `Cache-Control` | `no-store` | Responses carry user data; no cache keeps them |

The headers are set **before** the handler runs, so one endpoint can loosen
one - a `Cache-Control` on a public resource - without its own middleware.
`AllowCaching` leaves `Cache-Control` to handlers everywhere.

HSTS is always sent. A browser ignores it over plain HTTP (RFC 6797 §8.1), so
there is nothing to turn off in development - and a switch would stay off in
production. `hsts-preload` asks browsers to ship the domain as HTTPS-only;
it needs `includeSubDomains` and a year, and is hard to undo.

A service that serves HTML sets its own `content-security-policy` (start from
`default-src 'self'; frame-ancestors 'none'`) and usually
`cross-origin-resource-policy: same-origin`.

## CORS

- **Empty `allowed-origins` turns CORS off.** No `Access-Control-*` header is
  sent and a preflight reaches the handler. This is the fix for the services
  that allow any origin today; roll it out with their real allowlists.
- `https://*.example.com` matches one or more subdomain labels, with that
  scheme and port only - never `example.com`, never `evilexample.com`.
- `*` is for a public API without credentials, and must stand alone.
- `New` refuses `*` with credentials, credentials with no origins, `null`, a
  path or trailing slash, user info, and a wildcard over a TLD.
- The configured method and header lists are sent as they are; a request's
  `Access-Control-Request-Headers` is never echoed back.
- `Vary: Origin` is added (never set) on every response, so a cache does not
  hand one origin's answer to another.
- Every preflight is answered with 204 - from a refused origin, with nothing
  a browser accepts - so none reaches a handler.

**CORS is not authorization.** It decides which pages a browser lets read a
response, not who may call: a refused origin is still served. With
credentials, also refuse cross-origin state changes (CSRF) with Go's
`http.CrossOriginProtection`:

```go
csrf := http.NewCrossOriginProtection()
for _, origin := range trustedOrigins { // exact origins, no wildcards
    if err := csrf.AddTrustedOrigin(origin); err != nil {
        return err
    }
}
handler = headers.Wrap(cors.Wrap(csrf.Handler(mux)))
```

It refuses non-safe cross-origin browser requests using `Sec-Fetch-Site`, so
it needs no token.

## Several services

CORS is decided per response, by whatever answers the request. With many
services behind shared infrastructure:

- **One layer answers CORS, never two.** If the gateway (nginx, traefik)
  already adds `Access-Control-*` headers, leave `allowed-origins` empty in the
  services behind it: that is CORS off, and nothing is added. When a gateway
  and a service both send `Access-Control-Allow-Origin`, the browser sees two
  values and rejects the response. Pick the layer per route.
- **Services behind one API origin** (`api.aioz.io/stream`, `api.aioz.io/map`)
  are called by the same front ends, so they need the same allowlist. Keep it in
  one place in the deploy config and give every service the same value. A
  preflight is per URL, so a service with a shorter list fails only its own
  paths - a bug that looks like it comes and goes.
- **Services on their own subdomains** each list the front ends that call them.
  `https://*.aioz.io` covers every front end, and also every subdomain someone
  can put a page on: a dangling DNS record, a preview deployment, a host for
  user content. With `allow-credentials`, prefer the explicit list.
- **Cookies are about sites, not origins.** A front end on `app.aioz.io` and an
  API on `api.aioz.io` are one site: `SameSite=Lax` cookies are sent, and CORS
  is still needed because they are two origins. A front end on another domain
  needs `SameSite=None; Secure` cookies as well as CORS.
- **A shared cache or CDN** in front of several services is why `Vary: Origin`
  is always sent: it keeps one origin's answer from being served to another.
- `cross-origin-resource-policy: same-site` lets our sites on other
  subdomains embed each other's responses. Use `same-origin` for a service
  that must not be embedded even by them, and `cross-origin` only for a
  public asset host.

## One API, two audiences: a dashboard and SDK users

A service like stream is called by its own dashboard and by customers'
SDKs, on the customers' servers and in their users' browsers. No single
allowlist is right for both. What decides the CORS policy is how each caller
authenticates, not who it is:

- **A cookie the browser sends by itself** needs an exact origin allowlist
  with credentials, because any page the user visits can make the browser
  send it.
- **A token the caller puts in a header** is safe with `*`, because a page
  can only send a token it already has.

So such a service has more than one policy, one per group of routes:

| Caller | Authenticates with | CORS | Other headers |
| --- | --- | --- | --- |
| Our dashboard front end | session cookie | none, when the dashboard and its API share an origin (`app.x/api/*`, best); otherwise the exact dashboard origin with credentials | defaults, plus `http.CrossOriginProtection` |
| Server-side SDK (Node, Go, Python on the customer's backend) | secret API key in a header | none needed: servers do not send preflights or check CORS | defaults |
| Browser SDK or player on customers' sites (any origin) | publishable key or short-lived token in a header, never a cookie | `*`, no credentials | defaults |
| Media delivery (HLS manifests, segments, thumbnails) | signed, expiring URLs | `*`; allow `Range`; expose `Content-Length` and `Content-Range` | `AllowCaching`, `cross-origin-resource-policy: cross-origin` |
| Embeddable player page (an iframe) | signed URL, or the key's allowed domains | none: it is a page, not a fetch | framing allowed (see below) |
| Webhooks | a signature header | not applicable: server to server | not applicable |

### Why `*` is right for the public API

With `*` and no credentials, the browser never attaches cookies to a
cross-origin call, so a malicious site can do nothing with the API that it
could not do with curl. What protects a public API is the key design, not the
origin:

- **A secret key never reaches a browser.** A browser SDK gets a publishable
  key with a narrow scope (play, list public videos) or a short-lived token
  that the customer's backend mints with its secret key.
- **Browser uploads use a one-time upload URL or token,** created server-side
  for exactly one upload. The secret key stays on the customer's server.
- **Limits are per key and per address:** a [`ratelimit`](../ratelimit/README.md)
  rule on `Key(apiKeyID)` beside the `IPKey` rule.

### Letting a customer restrict a key to their sites

A publishable key can carry the origins its owner allows, as Stripe's,
Mapbox's and Algolia's do. The check cannot happen in the preflight, which
carries no `Authorization` header and so names no key: answer preflights
generally (`*`), then on the real request look the key up and refuse with 403
when the request's `Origin` is not in its list.

This keeps other *websites* from spending a customer's leaked publishable
key. It is quota protection, not authentication: anything that is not a
browser sends whatever `Origin` it likes. The check lives in the service's
API-key middleware; `CORS` knows only a static list.

### Wiring it

One policy per route group, each group wrapped separately:

```go
dashboardCORS, err := httpsec.NewCORS(httpsec.CORSConfig{
    AllowedOrigins:   cfg.DashboardOrigins, // exact; empty when the dashboard is same-origin
    AllowCredentials: true,
})
publicCORS, err := httpsec.NewCORS(httpsec.CORSConfig{
    AllowedOrigins: []string{"*"}, // header tokens only, never credentials
    AllowedHeaders: []string{"Authorization", "Content-Type", "X-Api-Key"}, // plus Range, tus headers if used
    ExposedHeaders: []string{"Retry-After", "X-Request-Id", "Location"},
})
mediaHeaders, err := httpsec.NewHeaders(httpsec.HeadersConfig{
    AllowCaching:              true,
    CrossOriginResourcePolicy: "cross-origin",
})
```

`Retry-After` and `X-Request-Id` are listed in `ExposedHeaders` because
neither is CORS-safelisted: without it, an SDK in a browser cannot read them.

**Embeddable pages.** `Headers` always sends `X-Frame-Options: DENY`, which
blocks a player iframe. The embed handler sets its own
`Content-Security-Policy` with `frame-ancestors *` (or the customer's
domains) and deletes `X-Frame-Options`; the headers are set before the
handler runs, so it can.

### Mistakes to avoid

- **One allowlist for everything.** Adding every customer's domain to it does
  not scale, and a new customer is broken until someone edits the config.
- **Credentials with a reflected `Origin`.** Any website can then read a
  logged-in user's data. `NewCORS` refuses `*` with credentials; reflecting
  the origin by hand does the same damage.
- **Cookies for the browser SDK.** They force credentialed CORS for unknown
  origins, which browsers refuse with `*` and which is unsafe to fake.
- **`no-store` or `same-site` on media.** Players on other sites cannot load
  the segments, and the CDN cannot cache them.

## API

```go
type HeadersConfig struct {
    ContentSecurityPolicy     string
    CrossOriginResourcePolicy string
    HSTSMaxAge                time.Duration
    HSTSOmitSubdomains        bool
    HSTSPreload               bool
    AllowCaching              bool
}

func NewHeaders(config HeadersConfig) (*Headers, error)
func (h *Headers) Apply(dst http.Header)
func (h *Headers) Wrap(next http.Handler) http.Handler

type CORSConfig struct {
    AllowedOrigins   []string
    AllowedMethods   []string
    AllowedHeaders   []string
    ExposedHeaders   []string
    AllowCredentials bool
    MaxAge           time.Duration
}

func NewCORS(config CORSConfig) (*CORS, error)
func (c *CORS) Handle(dst http.Header, r *http.Request) (preflight bool)
func (c *CORS) Wrap(next http.Handler) http.Handler

var Error = errs.Tag("httpsec")
```
