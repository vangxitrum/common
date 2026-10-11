# httpapi

The team HTTP API standard as code: response envelope, error body,
pagination, cache policy, ETags, request ids, access log, panic recovery and
body limit. One core on `net/http`, and an adapter each for echo and gin.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
import "gitlab.internal/tuan.quang.tran/aioz-common/httpapi/echoapi" // echo v4
import "gitlab.internal/tuan.quang.tran/aioz-common/httpapi/ginapi"  // gin v1
```

A survey of our services (2026-10-08) found three error bodies, three
pagination shapes and `Cache-Control` written as string literals in handlers,
across one gin and two echo services. The standard they should all follow is
team-playbook's `conventional/RESTfulAPI.md`; this package is the way to follow
it without remembering it.

## Setup

```go
stack, err := httpapi.NewStack(cfg.API) // validates at startup; a bad setting stops the service
if err != nil {
    return err
}
```

```go
e := echo.New()                 // echo
echoapi.Setup(e, log, stack)

r := gin.New()                  // gin
ginapi.Setup(r, log, stack)
```

`Setup` installs the stack in one order on both frameworks:

| # | What | Why there |
| --- | --- | --- |
| 1 | Client IP from the trusted proxies | Everything after logs and limits by the real address |
| 2 | Request id: a new UUIDv7 for every request; an `X-Request-Id` the caller sends is ignored, so no client can pick an id that sends an investigation down the wrong trail | On the response, the context and every `httpclient` call made while serving; always in `Access-Control-Expose-Headers`, so a frontend on an allowed origin can read it |
| 3 | `httpsec` headers and CORS | A preflight to any path is answered; every response has the headers, `Cache-Control: no-store` by default |
| 4 | Access log | Sees the status the error renderer finally wrote; aioz-stream's request line, below |
| 5 | Panic recovery | A panic is a classified 500, not a dead process |
| 6 | Body limit | Fixed at the first read, so a route's `BodyLimit` can still change it: a `Content-Length` over it fails before anything is read, an undeclared body fails at the limit; both are a 413 |
| 7 | Per-route metrics | Tagged by route pattern, never the raw path |

```yaml
api:
  trusted-proxies: [10.0.0.0/8]
  body-limit: 1048576
  cors:
    allowed-origins: [https://app.aioz.io]
```

### What a service can change

What a client sees is fixed: the envelope, the error body, `reason`, the
request id, the security headers and CORS have no switch (bar a page route's
Content-Security-Policy, below), because a standard
every service can bend is not a standard. What differs between services is how
the stack runs, and `Setup` takes options for that:

```go
echoapi.Setup(e, log, stack,
    httpapi.SkipLog("/health", "/ready"), // out of the access log unless they fail with a 5xx
)
```

| Option | Does |
| --- | --- |
| `SkipLog(paths...)` | Leaves these exact paths out of the access log unless the status is 5xx, so a load balancer's probe does not bury real traffic. They still count in the metrics. |
| `WithoutAccessLog()` | No access log, for a service that writes its own. Server faults are still logged with their cause. |
| `WithoutRecovery()` | No panic recovery, for one that installs its own, for example to report to an error tracker. Return `httpapi.Recovered(v)` from it to keep the standard 500. |
| `WithoutMetrics()` | No per-route metrics, for one that records its own. |

A route whose bodies are a different size from the stack's sets its own limit:

```go
api.POST("/videos/upload", upload, echoapi.BodyLimit(512<<20))           // echo
api.POST("/videos/upload", ginapi.BodyLimit(512<<20), ginapi.Handle(upload)) // gin
api.POST("/webhooks/stripe", ginapi.BodyLimit(64<<10), ginapi.Handle(hook))  // lower works too
```

A limit of zero or less panics at registration. `httpapi.SetBodyLimit` is the
same for a plain `net/http` handler.

A route that serves a page sets its own Content-Security-Policy. The stack's
lets a page load nothing, so without this Swagger UI renders blank:

```go
r.GET("/swagger/*any", ginapi.ContentSecurityPolicy(httpsec.SwaggerUIPolicy), ginSwagger.WrapHandler(swaggerFiles.Handler)) // gin
e.GET("/swagger/*", echoSwagger.WrapHandler, echoapi.ContentSecurityPolicy(httpsec.SwaggerUIPolicy))                    // echo
```

Every other route keeps the strict policy. See
[`httpsec`](../httpsec/README.md#a-different-policy-for-some-routes).

## Handlers

```go
// echo
func getVideo(c echo.Context) error {
    v, err := svc.Video(c.Request().Context(), c.Param("id"))
    if err != nil {
        return err // an apierr renders as asked; anything else is a 500 that leaks nothing
    }
    if echoapi.CheckETag(c, httpapi.ETag(v.Version)) {
        return nil // 304, no body
    }
    return echoapi.OK(c, toDTO(v))
}
api.GET("/videos/:id", getVideo, echoapi.Cache(httpapi.CachePolicy{Type: httpapi.Revalidate}))

// gin: a handler that returns an error, through ginapi.Handle
api.GET("/videos/:id", ginapi.Cache(httpapi.CachePolicy{Type: httpapi.Revalidate}), ginapi.Handle(getVideo))
```

| Helper (echoapi and ginapi) | Does |
| --- | --- |
| `OK`, `Created` | The success envelope |
| `List(c, "videos", items, total, query)`, `CursorList(c, key, items, next, query)` | One page in aioz-stream's shape: items under their name and `total` (or `next_cursor`) in `data`, and beside it `query` as served, defaults applied; a nil list is `[]`, never `null` |
| `Send(c, status, httpapi.Message("..."))` | A success with only a message for a person; `Success(v).WithMessage("...")` for both |
| `Page(c, httpapi.PageConfig{})` | `offset`/`limit`/`cursor`, as aioz-stream reads them: default 25, a non-positive limit is the default; over 100, a bad integer or a negative offset is a 400 (`limit-too-large`, `invalid-limit`, `invalid-offset`) |
| `Bind(c, &dto)` | Strict JSON: must be `application/json` (415), one value, no unknown fields (400 naming the field); the parser's text never reaches the client |
| `Bind(c, &dto, httpapi.AllowUnknownFields())` | The same, but fields `dto` does not have are ignored, for clients that may send a field before the server knows it (a mobile app released ahead of the API). Strict stays the default because it is what catches a misspelled field. |
| `BodyLimit(n)` | The route's body limit in place of the stack's, higher or lower |
| `Cache(policy)`, `NoStore()` | The route's cache policy; an invalid policy panics at registration |
| `CheckETag(c, etag)` | Sets `ETag`; answers 304 when `If-None-Match` matches |

## Bodies

```json
{"status":"success","data":{"id":"v1"}}
{"status":"success","message":"password changed"}
{"status":"success","message":"video published","data":{"id":"v1"}}
{"status":"success","data":{"videos":[...],"total":53},"query":{"offset":0,"limit":25}}
{"status":"success","data":{"events":[...],"next_cursor":"..."},"query":{"offset":0,"limit":25}}
{"status":"error","reason":"video-not-found","message":"no such video","request_id":"..."}
{"status":"error","reason":"unknown-field","message":"...","request_id":"...","details":[{"field":"nam","reason":"unknown-field"}]}
```

The error body is aioz-stream's existing shape, so its clients keep working.
`reason` is the field a client branches on. A 5xx always says
`an unexpected error occurred`; the cause, the packages it passed through, its
stack and its subject fields go to the log with the request id. An error
response is always `Cache-Control: no-store` with no `ETag`, whatever the
route's policy: a 404 must not be cached for five minutes.

`httpapi.WithDetails(apierr.BadRequest(...), httpapi.Detail{Field, Reason})`
names the fields a client must fix.

## Request log

One record per request, at info, in the group `api`, in aioz-stream's format:

```
GET status:200 latency:012ms url:/v1/videos/42   group=api request_id=... route=/v1/videos/:id remote_ip=... bytes=56 user_agent=...
PST status:201 latency:003s url:/v1/videos
GET status:404 latency:155µs url:/v1/nope        ... reason=route-not-found
GET status:500 latency:012ms url:/v1/plain       ... reason=internal err="db down: ..."
```

The method is three letters (`PST`, `PTC`, `DEL`) so a tailed log lines up, and the method and status are ANSI-colored as aioz-stream colors them. The logger's Graylog and Elasticsearch sinks strip the colors and file the line as fields (method, status, latency, url), so dashboards built on aioz-stream's logs work unchanged. `request-log-plain: true` writes it without colors, for a plain-text file. The url is the path only: a query string can carry a token. A failed request adds its `reason`, and a server fault its `err` text; `LogError` logs the fault again with its stack.

## Cache policies

| Type | `Cache-Control` | For |
| --- | --- | --- |
| `Public` + `MaxAge` | `public, max-age=N` (+ `stale-while-revalidate`) | The same for everyone: catalogs, public media metadata |
| `Public`, no `MaxAge` | `public, no-cache` | The same for everyone, changes any time; pair with an ETag |
| `Private` + `MaxAge` | `private, max-age=N`, `Vary: Authorization` | The caller's own, fine to reuse briefly |
| `Revalidate` | `private, no-cache`, `Vary: Authorization` | The caller's own, changes any time, worth a 304 |
| `NoStore` (and no policy) | `no-store` | Tokens, personal data, money, errors |

Build an ETag from what the resource is - a row version, `updated_at` in
nanoseconds, a content hash - never from the rendered body.

## Why two modules more

`httpapi/echoapi` and `httpapi/ginapi` are Go modules of their own:

- A service on echo never downloads gin, and the other way round.
- gin v1.12 needs quic-go 0.59, which breaks the libp2p WebTransport code that
  `aioz-common/depin` reaches through go-sdk. In the root module it broke the
  build. **A gin service that also imports `depin` hits the same conflict**;
  pin quic-go to what go-sdk needs in that service until go-sdk moves.

The adapters import `httpapi/internal/conformance`, the suite both must pass:
the same requests, the same statuses, headers and bodies. A change to either
adapter, or to the core, runs it for both (`make test-adapters`, and the
`modules` CI job).

### Releasing

An adapter's `go.mod` has `replace gitlab.internal/tuan.quang.tran/aioz-common => ../../`
while the core it needs is unreleased. To release:

1. Tag aioz-common (`vX.Y.Z`) with the core change.
2. In each adapter, drop the `replace`, `go get gitlab.internal/tuan.quang.tran/aioz-common@vX.Y.Z`, tidy, merge.
3. Tag each adapter on that commit: `httpapi/echoapi/vA.B.C`, `httpapi/ginapi/vA.B.C`.
