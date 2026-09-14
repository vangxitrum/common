# ratelimit

How often one identity - an account, an address, an email - may do one thing,
across every replica of a service.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/ratelimit"
```

Most of our limiters today are a map in one process. Each replica keeps its
own count, so four replicas allow four times the limit, a restart forgets
everyone, and hub's map has no mutex at all. This one keeps its state in Redis,
falls back to memory when Redis is down, and answers every request with an
exact `Retry-After`.

## Usage

```go
limiter, err := ratelimit.New(log, cfg.RateLimit, ratelimit.WithRedis(redisClient)) // no option: memory only
if err != nil {
    return err
}

var signupIP = ratelimit.Rule{
    Name:  "signup-ip",
    Rates: ratelimit.Rates{{Count: 5, Period: time.Minute}, {Count: 50, Period: time.Hour}},
}

result, err := limiter.Allow(ctx, signupIP.IPKey(resolver.ClientIP(r)))
if err != nil {
    http.Error(w, "try again later", http.StatusServiceUnavailable)
    return
}
if !result.Allowed {
    w.Header().Set("Retry-After", strconv.FormatInt(result.RetryAfterSeconds(), 10))
    http.Error(w, "too many requests", http.StatusTooManyRequests)
    return
}
```

```yaml
rate-limit:
  prefix: ratelimit      # namespace in a shared redis
  on-error: local        # local | open | closed
  timeout: 200ms         # bound on each redis call
  capacity: 100000       # windows the memory store remembers
```

A service can carry its limits in config too: `Rates` is a flag value,
`default:"5/1m,50/1h"`.

`Rate{Count: 5, Period: time.Minute}` is a burst of 5, then one every 12s as
the window refills. Several rates in one rule are checked together: a short
window against bursts, a long one against a slow, steady attack.

## A delay, not a lockout

A denied request consumes nothing. An attacker hammering a victim's account
cannot push the victim's refill further out, so no one can be locked out on
purpose; the long window is the progressive delay the OWASP Credential
Stuffing cheat sheet asks for.

When several keys are passed to one `Allow`, the request is counted only if
every window of every key allows it, and a denial in one counts nothing in the
others.

## Login, forgot-password and OTP

Limit **before** looking the user up. A limit that runs after the lookup, or
only for accounts that exist, answers "does this email have an account?" to
anyone who asks. The 429 must be the same either way.

```go
var (
    loginIP        = ratelimit.Rule{Name: "login-ip", Rates: ratelimit.Rates{{Count: 20, Period: time.Minute}, {Count: 200, Period: time.Hour}}}
    loginAccountIP = ratelimit.Rule{Name: "login-account-ip", Rates: ratelimit.Rates{{Count: 5, Period: time.Minute}, {Count: 20, Period: time.Hour}}}
    loginAccount   = ratelimit.Rule{Name: "login-account", Rates: ratelimit.Rates{{Count: 100, Period: time.Hour}}}
)

addr, err := mail.ParseAddress(form.Email)
if err != nil {
    // a 400, before any limit: an empty key is an error, not a shared bucket
}
email := addr.Canonical() // one account, one bucket, however it is spelled
ip := resolver.ClientIP(r)

account := loginAccount.Key(email)
accountIP := loginAccountIP.Key(email + " " + ip.String())

result, err := limiter.Allow(ctx, loginIP.IPKey(ip), accountIP, account)
// 503 on err, 429 with Retry-After when denied - then, and only then, look the
// user up and verify the password.

// On success, forgive the account's keys. Never the address's: an attacker
// with one valid account would reset the limit on the address they attack from.
_ = limiter.Reset(ctx, accountIP, account)
```

| Rule | Key | Rates | Stops |
| --- | --- | --- | --- |
| `login-ip` | `IPKey(ip)` | 20/1m, 200/1h | one address spraying many accounts |
| `login-account-ip` | account + address | 5/1m, 20/1h | guessing one password from one place |
| `login-account` | account | 100/1h | a botnet guessing one password from many places |
| `otp-email`, `reset-email` | `Key(email)` | 1/1m, 5/1h | mail bombing and code guessing |
| `otp-ip`, `reset-ip` | `IPKey(ip)` | 10/1m, 50/1h | one address walking the email list |

Build the address with [`clientip`](../clientip). `X-Forwarded-For` read any
other way lets a client choose the address it is limited by.

## When Redis fails

Every Redis call is bounded by `timeout`, even against a server that accepts
connections and never answers (go-redis alone would wait out the socket read
timeout on each retry). Past it, `on-error` applies:

| `on-error` | Behavior | Choose it when |
| --- | --- | --- |
| `local` (default) | limit in this replica's memory | protection matters and an outage is rare |
| `open` | allow everything | the limit is a courtesy, availability comes first |
| `closed` | `Allow` returns an error; answer 503 | a missed limit costs money (SMS, email sends) |

The failure is logged once when it starts and once when Redis answers again,
never per request. `ratelimit_store_failed` counts every one.

## One Redis, many services

Most of our Redis servers serve several services, and that changes two things.

**Give each service its own `prefix`.** Keys are
`<prefix>:{<rule>:<digest>}:<rate>`, and the prefix defaults to `ratelimit`. Two
services with a `login-ip` rule and the default prefix share one bucket: stream's
logins spend map's budget. Set `rate-limit.prefix: stream` in one and
`rate-limit.prefix: map` in the other. Share a prefix only on purpose, when
several services should draw on one budget, such as two front ends to the same
accounts.

```yaml
rate-limit:
  prefix: stream
```

**Keep limits off a cache that evicts.** Eviction is per server, not per
database, and every limiter key has a TTL, so a cache Redis running
`allkeys-lru` or `volatile-lru` drops counts whenever it is full. Put the
limiter on a server with `noeviction` (the session or queue Redis usually is),
or give the cache its own server.

The rules named in code (`login-ip`, `otp-email`) need no coordination between
services: the prefix keeps them apart.

## Keys

- The value is hashed (SHA-256, 128 bits kept) the moment the key is built. No
  email or address is stored in Redis or logged, and an attacker-sized value
  cannot make an attacker-sized key.
- An IPv4-mapped IPv6 address is its IPv4 address. An IPv6 address is keyed by
  its /64, since one subscriber is usually handed a whole /64.
- A key expires the moment its window would be full again, so Redis holds
  nothing for a client that stopped.
- Changing a rule's rates starts its windows fresh.

## Limits

- A distributed attack can still spend one account's `login-account` budget,
  which then delays the real user too. The next step up is a CAPTCHA or an
  emailed sign-in link, which this package does not do.
- One Redis node. The windows of one call can fall in different Redis Cluster
  slots.
- Redis must run with `noeviction`. Every limiter key has a TTL, so
  `volatile-*` policies evict them as readily as `allkeys-*` do, and an
  evicted count is a reset limit.
- Time is each replica's clock. Replicas a second apart disagree on windows by
  a second.

## Metrics

`ratelimit_allowed` and `ratelimit_denied`, tagged `rule`;
`ratelimit_store_failed`, tagged `mode`; `ratelimit_evicted` from the memory
store. No series carries a key.

## API

```go
type Config struct {
    Prefix   string
    OnError  string        // OnErrorLocal, OnErrorOpen, OnErrorClosed
    Timeout  time.Duration
    Capacity int
}

func New(log *slog.Logger, config Config, opts ...Option) (*Limiter, error)
func WithRedis(client *redis.Client) Option
func (l *Limiter) Allow(ctx context.Context, keys ...Key) (Result, error)
func (l *Limiter) Reset(ctx context.Context, keys ...Key) error

type Rate struct{ Count int; Period time.Duration }
type Rates []Rate // flag.Value: "5/1m,20/1h"
type Rule struct{ Name string; Rates Rates } // name: a-z, 0-9, -; at most 32
func (r Rule) Key(value string) Key
func (r Rule) IPKey(addr netip.Addr) Key

type Result struct{ Allowed bool; RetryAfter time.Duration }
func (r Result) RetryAfterSeconds() int64

var Error = errs.Tag("ratelimit")
```
