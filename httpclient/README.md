# httpclient

`http.DefaultClient` has no timeout. A dependency that accepts a connection and
then never answers holds the calling goroutine, its connection and its request
for as long as the process lives, so a service that calls one on a request path
dies of a fault in something it merely talks to.

The second failure is quieter. `http.DefaultTransport` keeps two idle
connections per host, so at any real concurrency a service opens and discards a
connection - and pays a TLS handshake - on nearly every request. That turns a
five millisecond dependency into a fifty millisecond one, and it is reported as
"the network is slow" rather than as a setting nobody chose.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
```

## Usage

```go
billing, err := httpclient.New(log, cfg.Billing)
if err != nil {
    return err
}
group.Add(lifecycle.Item{Name: "billing", Close: func() error {
    billing.CloseIdleConnections()
    return nil
}})

req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
if err != nil {
    return err
}
resp, err := billing.Do(req)
```

There is no `monkit.Default.ScopeNamed(...).Chain(...)` line here, and its
absence is deliberate - see below.

## One client per dependency

`Name` is what the metrics are tagged by, so build a client per dependency
rather than one for the process:

```go
billing, err := httpclient.New(log, cfg.Billing)   // Name: "billing-api"
ledger, err := httpclient.New(log, cfg.Ledger)     // Name: "ledger"
```

That is not only about labels. Each dependency then gets its own connection
pool and its own timeout, so a slow one cannot exhaust the pool the fast one is
using - which is the same reason `redis.Config` carries a name.

## `New` is the one to reach for; `NewOneShot` states its own precondition

`go-cleanhttp` gets this the wrong way round: the profile it calls
`DefaultTransport` is the unusual one, and its own documentation has to warn
that using it as a default leaks file descriptors. Here the pooled client is
`New`, because that is what almost every call site wants, and the transient
profile is `NewOneShot` - a name that cannot be mistaken for a default.

There is no `keep-alives` flag, on purpose. If there were, you could turn a
long-lived client into a transient one by editing YAML, and the choice would
stop being visible in the code that makes it.

## The timeout is a backstop; the context is the bound

`Timeout` catches the caller who forgot a deadline. It is not the mechanism:
pass a context with a deadline, because that is the only thing that also bounds
the work either side of the request.

`go-cleanhttp` sets no client timeout at all and relies on the caller. This
package sets one because a shared library that assumes every call site
remembers is a shared library that will be part of an incident.

## Retry is opt-in, per call site

Off by default, for two reasons. A client that retries by default triples the
load on a dependency that is already failing. And a POST that timed out may
well have succeeded, so replaying it charges the card twice.

```go
resp, err := client.Do(req.WithContext(httpclient.WithRetry(ctx)))
```

The layer is always installed and always reads the context, so one client - and
one connection pool - serves both the calls that should retry and the calls
that must not. `WithoutRetry` is the inverse, for a client that retries by
default.

What is retried: connection errors that are not certificate failures, `429`, and
`5xx` except `501`, which is a permanent statement about the endpoint. And only
if the request can be replayed at all: `GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT`
and `DELETE`, or anything carrying an `Idempotency-Key`.

**A retrying call site must set a deadline that covers every attempt.** Retry is
a `RoundTripper`, so it sits inside `Client.Timeout` and inside the context
deadline and all the attempts share one budget. A one-second deadline buys no
useful retry. That is correct rather than a bug - the deadline is your statement
of how long you are willing to wait - but it surprises people.

A request body is replayed through `req.GetBody`, which `http.NewRequest` fills
in for `*bytes.Reader`, `*bytes.Buffer` and `*strings.Reader`. A body from some
other `io.Reader` is simply not retried; nothing is buffered up front to pretend
otherwise, which is what `go-retryablehttp` does to every request whether a
retry happens or not.

## `Retry-After` is obeyed, up to a point

Both forms are read: a count of seconds and an HTTP date. The server knows more
than any backoff can compute, so its answer replaces the computed wait.

A `Retry-After` longer than `RetryWaitMax` ends the retrying and hands the
response back with its body intact. Sleeping for as long as a remote asks is a
way for whoever operates that remote to hold this service's goroutines and its
pooled connections.

## A discarded response is drained before it is closed

`http.Transport` returns a connection to the idle pool only once its body has
been read to EOF. Closing a `503`'s body early makes the transport close the TCP
connection instead - so a service retrying through an incident would also be
destroying its own connection pool and adding a fresh TLS handshake to every
attempt, at exactly the moment it was trying to survive one.

The drain is bounded at 4KB. An error page bigger than that is not read to the
end and its connection is dropped, which is the better trade: copying a megabyte
to `io.Discard` to save one handshake is not worth it. Both halves have a test.

## An over-long body says so, rather than truncating

`MaxResponseSize` fails the read at the first byte over the limit.

The obvious implementation - `io.LimitReader`, which is what
[`readcloser.LimitReadCloser`](../readcloser/README.md) wraps - ends an
over-long body in `io.EOF` instead. A forty megabyte response truncated at
thirty-two would reach the caller as `unexpected end of JSON input` with
nothing to say a cap existed. Silently truncating a response is worse than the
unbounded read the cap was meant to prevent.

## Redirects: a cap you choose, and no downgrade

What Go already does, so this package does not claim it: with no policy it stops
after ten redirects, and since Go 1.8 it strips `Authorization`,
`WWW-Authenticate` and `Cookie` when a redirect crosses to another domain.

This adds two things. `MaxRedirects` is configurable, and `0` returns the `3xx`
response rather than erroring - a caller that wants the `Location` header wants
the response, and making zero mean "fail" would leave no way to say "do not
follow". And an `https` to `http` redirect is refused, because Go will follow
one and everything the request carries then crosses the wire in clear text.

## Metrics are tagged by client, never by URL

`http_request_duration` carries the client name and nothing else.
`http_request` is a counter tagged by method and status class, whose whole cross
product is created at construction so the hot path is two array indexes rather
than a locked map lookup.

The host is **not** a tag. A client talking to per-tenant hostnames, or
bucket-in-host S3 URLs, or service-discovery addresses would produce an
unbounded number of series, and monkit has no cardinality limiter that would
catch it. One client per dependency is the better breakdown anyway. `HostTag`
exists for a genuinely fixed, small set of hosts, and adds the tag to the
counter only - never to the histogram.

No URL, path or query string appears in any series key or in any error message
this package writes: a path carries ids and a query string carries tokens. There
is a test that asserts it.

Because retry sits above metrics, one sample is one attempt on the wire - the
dependency's real latency - rather than a total with two backoffs folded
invisibly into it. Because `http.Client` handles redirects above the whole
chain, a redirect chain produces one sample per hop.

## There is no `StatSource`, and no `Chain` call

An `http.Transport` exports no pool counters, so there is nothing to poll. Every
number this package has is pushed at the moment it happens, and
`monkit.Package()` has already registered it. `redis.Client.Stats` exists only
because `PoolStats()` is a gauge that does not exist unless someone asks for it.

## What this package does not do

There is no server, no router and no middleware here, and there will not be:
services are not required to share an HTTP framework, so anything tied to one
would be unusable by half of them. What they do share is the outbound side,
because `*http.Client` is the type every framework and every vendored SDK
already accepts.

It is also not a circuit breaker and not a rate limiter. It does not parse
bodies, does not sign requests, and defines no `Client` interface - `New`
returns the standard library's own type.

## API

```go
type Config struct{ ... }

func (config Config) Validate() error

func New(log *slog.Logger, config Config) (*http.Client, error)
func NewOneShot(log *slog.Logger, config Config) (*http.Client, error)
func NewWith(log *slog.Logger, config Config, base http.RoundTripper) (*http.Client, error)

func Transport(config Config) (*http.Transport, error)
func OneShotTransport(config Config) (*http.Transport, error)

func WithRetry(ctx context.Context) context.Context
func WithoutRetry(ctx context.Context) context.Context

const RequestIDHeader = "X-Request-Id"

func WithRequestID(ctx context.Context, id string) context.Context
func RequestID(ctx context.Context) string

var Error = errs.Tag("httpclient")
```

## Tests

```bash
go test ./httpclient/...
```

`httptest` only: no docker, no network, and no sleeping - the retry timings are
driven with a [`time2`](../time2/README.md) machine.
