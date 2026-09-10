# redis

A configured, observable go-redis client that fails at startup when it cannot
reach the server.

go-redis dials lazily. A service with a typo in its address, an expired
password or a firewall in the way starts perfectly cleanly and reports itself
healthy; the failure surfaces on the first business request, hours later,
attributed to whatever code happened to make it. `Open` dials and pings, so a
misconfigured cache fails next to the configuration that caused it.

The second thing it fixes is observability. Across our services, exactly three
options are ever set - address, password, database. Nothing sets a timeout, a
pool size or a retry count, nothing has a single redis metric, and nothing
calls `Close`. Every one of those is a field or a method here, and the metrics
arrive without anyone remembering to add them.

```go
import (
    goredis "github.com/redis/go-redis/v9"

    "gitlab.internal/tuan.quang.tran/aioz-common/redis"
)
```

## Usage

```go
cache, err := redis.Open(ctx, log, cfg.Redis)
if err != nil {
    return err
}
monkit.Default.ScopeNamed("redis").Chain(cache)

group.Add(lifecycle.Item{Name: "redis", Close: cache.Close}) // Run is nil

val, err := cache.Get(ctx, "key").Result()
if errors.Is(err, goredis.Nil) {
    // a miss
}
```

`Client` embeds `*redis.Client`, so every command is reachable directly and
nothing is wrapped. `Health` pings on demand, for a readiness probe. `Stats` is
a `monkit.StatSource` over the pool.

## `redis.Nil` is a miss, not an error

`Get` on a key that is not there returns `goredis.Nil`. It is the normal
outcome of a cache lookup, not a failure, and the metrics hook here knows the
difference: a miss is not counted as an error. Callers need the same
distinction, which is why `errors.Is(err, goredis.Nil)` belongs on every read
path before the error is logged.

## Both packages are called redis

This one and the library. Import the library under an alias - `goredis` is the
one used here - or the compiler picks the local package and `goredis.Nil` is
unreachable.

## One pool per logical database

`DB` selects a logical database, and two databases on one server are two
`Open` calls. Give each a distinct `Name`: it tags the metrics, and without it
the two pools' series are indistinguishable.

## A DSN is a credential

`URL` embeds the password, so the whole field is tagged `secret:"optional"` and
`SaveConfig` writes it blank. When it is set it overrides `Address`, `DB`,
`Username` and `Password` entirely - never half of them. The timeout, pool and
retry fields still apply on top of a parsed URL, so a DSN never silently
reverts a timeout to the library default.

## No cluster support, and why

`Client` embeds the concrete `*redis.Client`. Supporting a cluster would mean
embedding `UniversalClient` instead, an interface, and every caller would lose
the concrete methods. It is also not a silent swap: `SCAN` and `SSCAN` sweeps
do not behave the same against a cluster. If it is ever needed it arrives as a
separate `OpenCluster` returning a distinct type.

## What this package does not do

It wraps no commands and defines no `Cache` interface. Reconnection, pooling
and per-command retry are go-redis's own. Pub/sub, streams, Lua and locking are
reachable through the embedded client and are not re-exported here.

## API

```go
type Config struct {
    Name string // tags this client's metrics

    URL      string // redis:// or rediss://; overrides the four fields below
    Address  string
    DB       int
    Username string
    Password string

    ClientName string

    PoolSize     int
    MinIdleConns int
    MaxRetries   int // -1 disables retrying

    DialTimeout  time.Duration
    ReadTimeout  time.Duration // -1 disables
    WriteTimeout time.Duration // -1 disables
    PoolTimeout  time.Duration // 0 uses ReadTimeout+1s

    TLS           bool
    TLSSkipVerify bool // test use only

    RequireOnStart bool          // refuse to start if unreachable
    HealthTimeout  time.Duration // bounds the startup ping and Health

    Metrics bool
}

func (config Config) Options() (*goredis.Options, error)

func Open(ctx context.Context, log *slog.Logger, config Config) (*Client, error)
func OpenWith(ctx context.Context, log *slog.Logger, opts *goredis.Options, config Config) (*Client, error)

func (client *Client) Health(ctx context.Context) error
func (client *Client) Close() error
func (client *Client) Stats(cb func(key monkit.SeriesKey, field string, val float64))
```

## Tests

The container tests skip when there is no docker daemon. Set
`AIOZ_REQUIRE_DOCKER=1` to turn that skip into a failure, which is what CI
does:

```
AIOZ_REQUIRE_DOCKER=1 go test -race ./redis/...
```
