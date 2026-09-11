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

## `NewOneShot` refuses internal addresses; `New` can be told to

A client aimed at a URL someone else supplied - a webhook, a callback, an image
to fetch - can be aimed at `http://169.254.169.254/`, the cloud metadata
endpoint that hands out the instance's credentials, or at any service on the
internal network. That is server-side request forgery. `NewOneShot` is the
client for exactly those URLs, so it refuses internal addresses unless told
otherwise. `New` is for a dependency named in configuration, which is usually
internal, so it allows them unless told otherwise.

```yaml
private-networks: block            # or allow; empty lets the constructor choose
allowed-networks: [10.20.0.0/16]   # exempt these, block the rest
```

- The check is on the address being dialed, after DNS resolution, not on the
  URL's host. A host check is defeated by any name that resolves inside, and by
  DNS rebinding, where one name answers public when checked and private when
  connected to.
- Every redirect hop is a new dial, so a public URL that redirects inside is
  refused at the hop that goes inside.
- Blocked: loopback, private, link-local, carrier-grade NAT, unique-local and
  site-local IPv6, multicast, reserved and documentation ranges, and an internal
  IPv4 address written as IPv4-mapped, NAT64 or 6to4 IPv6.
- Setting `allowed-networks` turns blocking on, even for `New`: an allowlist on
  a client that allows everything would mean nothing. Only
  `private-networks: allow` turns the guard off.
- A guarded transport does not use `HTTP_PROXY`. Through a proxy the guard
  would only ever see the proxy's address, and the proxy would connect wherever
  the URL says.
- A refusal wraps `ErrBlockedAddress`, is never retried, and counts
  `http_dial_refused`, tagged by client and reason.
- `NewWith` over a transport you built is guarded only if that transport came
  from `Transport` or `OneShotTransport`.

## The timeout is a backstop; the context is the bound

`Timeout` catches the caller who forgot a deadline. It is not the mechanism:
pass a context with a deadline, because that is the only thing that also bounds
the work either side of the request.

`go-cleanhttp` sets no client timeout at all and relies on the caller. This
package sets one because a shared library that assumes every call site
remembers is a shared library that will be part of an incident.

## Retrying is the caller's job

The client never retries: every `Do` is one attempt. A client that retries on
its own triples the load on a dependency that is already failing, and a POST
that timed out may well have succeeded, so replaying it charges the card twice.
Whether a call is safe and worth repeating is known at the call site, so that is
where retrying happens, in the open:

```go
policy := httpclient.Retry{Name: "billing", MaxAttempts: 4}

resp, err := policy.Do(ctx, billing, func(ctx context.Context) (*http.Request, error) {
    return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
})
```

`Do` calls the function once per attempt, so a body is sent whole every time -
build it inside the function, from whatever reader you like. Nothing is buffered
up front to replay it, which is what `go-retryablehttp` does to every request
whether a retry happens or not.

### Before you retry a call

1. **Is it safe to send twice?** `GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT` and
   `DELETE` are. A `POST` or `PATCH` is only when it carries an
   `Idempotency-Key` the server honors; without one it gets exactly one attempt,
   whatever `MaxAttempts` says. `AllowNonIdempotent` overrides that, for an
   endpoint you know is idempotent.
2. **Does the deadline cover every attempt?** All the attempts run under one
   context and share its deadline, and `Client.Timeout` bounds each of them.
   With the default wait, three retries spend up to 1+2+4 seconds waiting on top
   of the requests themselves, so a one-second deadline buys no useful retry. A
   wait that cannot be followed by an attempt before the deadline is not slept:
   you get the last real answer instead of a bare deadline exceeded.
3. **Can you see it working?** Each retry counts `http_retries`, tagged by
   `Name`, and giving up counts `http_retries_exhausted`. With `Log` set, each
   retry also writes one warn line with the method and the host.

### What is retried: `Retryable`

`Retryable` sees each attempt's response and error, and says whether to try
again. `DefaultRetryable` retries connection errors that are not certificate
failures or blocked addresses, `429`, and `5xx` except `501`, which is a
permanent statement about the endpoint. A rule of your own composes with it:

```go
policy.Retryable = func(resp *http.Response, err error) bool {
    if resp != nil && resp.StatusCode == http.StatusConflict {
        return true // this dependency's "busy, try again"
    }
    return httpclient.DefaultRetryable(resp, err)
}
```

It sees the status and the headers, never the body. A decision that needs the
body - a `200` that says `{"status":"busy"}` - belongs in a
[`retry.Backoff`](../retry/README.md) loop around the whole call, read and
decode included. The caller's context ending is never retried, whatever
`Retryable` says.

### How long to wait: `Wait`

`Wait` returns the sleep before the next attempt, given the zero-based index of
the attempt that failed and what it produced. Three come with the package:

| `Wait` | Sleeps |
| --- | --- |
| `Exponential(min, max)` | a random time below a nominal wait that doubles from `min` up to `max` |
| `Constant(d)` | `d`, every time |
| `RetryAfter(fallback)` | the server's `Retry-After`, or what `fallback` says when it sent none |

The default is `RetryAfter(Exponential(time.Second, 30*time.Second))`. The
randomness in `Exponential` is the point: when a dependency restarts, every
instance talking to it fails in the same instant, and without jitter they all
retry in the same instant too, through every doubling.

Any function of the right shape will do:

```go
policy.Wait = func(attempt int, _ *http.Response, _ error) time.Duration {
    return time.Duration(attempt+1) * 500 * time.Millisecond // linear
}
```

### `Retry-After` is obeyed, up to a point

Both forms are read: a count of seconds and an HTTP date. The server knows more
than any backoff can compute, so `RetryAfter` uses its answer in place of the
fallback.

Sleeping for as long as a remote asks is a way for whoever operates that remote
to hold this service's goroutines and its pooled connections. So
`DefaultRetryable` gives up on a `Retry-After` longer than `MaxRetryAfter` (30s)
and hands the response back with its body intact; `RetryableWithin(limit)` is
the same rule with another limit. The bound sits in `Retryable`, which decides
whether to retry, so that `Wait` only ever answers how long.

### Out of attempts

`Do` returns the last attempt's response and error exactly as the client
returned them. Out of attempts, you read the final `503` and its body rather
than an error that hides it. A cancelled context returns the context's error and
no response, because an answer to an abandoned request is not one.

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

A retry is a new call, so one sample is one attempt on the wire - the
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

type Retry struct {
    Name               string
    MaxAttempts        int
    Retryable          Retryable
    Wait               Wait
    AllowNonIdempotent bool
    Log                *slog.Logger
}

func (r Retry) Do(ctx context.Context, client *http.Client,
    newRequest func(ctx context.Context) (*http.Request, error)) (*http.Response, error)

type Retryable func(resp *http.Response, err error) bool
type Wait func(attempt int, resp *http.Response, err error) time.Duration

func DefaultRetryable(resp *http.Response, err error) bool
func RetryableWithin(limit time.Duration) Retryable
func ParseRetryAfter(resp *http.Response) (wait time.Duration, ok bool)

func Exponential(minWait, maxWait time.Duration) Wait
func Constant(d time.Duration) Wait
func RetryAfter(fallback Wait) Wait

const DefaultMaxAttempts = 4
const MaxRetryAfter = 30 * time.Second

const RequestIDHeader = "X-Request-Id"

func WithRequestID(ctx context.Context, id string) context.Context
func RequestID(ctx context.Context) string

var Error = errs.Tag("httpclient")
var ErrBlockedAddress = errors.New("address blocked by the private-network guard")
```

## Tests

```bash
go test ./httpclient/...
```

`httptest` only: no docker, no network, and no sleeping - the retry timings are
driven with a [`time2`](../time2/README.md) machine.
