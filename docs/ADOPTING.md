# Adopting aioz-common in a service

Each package documents itself. This is the part no single package README can
say: what a service wires up on day one, in what order, and which of the
choices are actually load-bearing.

```bash
go get gitlab.internal/tuan.quang.tran/aioz-common
```

## The shape of a service

```go
// At init, so the flags exist before cobra parses them.
config.Bind(cmd, &cfg, config.ConfDir(dir))

func run(ctx context.Context, cmd *cobra.Command) error {
    // 1. Configuration, before anything that needs it.
    if _, err := config.Load(cmd); err != nil {
        return err
    }
    if err := config.CheckSecrets(&cfg); err != nil {
        return err   // refuse to start rather than run on an empty key
    }

    // 2. Logging, so everything after this is observable.
    handler, _, err := logging.New(cfg.Logging)
    if err != nil {
        return err
    }
    log := slog.New(handler)

    // 3. Metrics, including the build stamp.
    monitor.Register(nil)
    version.Register(nil)

    // 4. Components, started together and closed in reverse.
    group := lifecycle.NewGroup(log)
    group.Add(lifecycle.Item{
        Name:  "debug",
        Run:   debugServer.Run,
        Close: debugServer.Close,
    })

    runners, ctx := errgroup.WithContext(ctx)
    group.Run(ctx, runners)
    return runners.Wait()
}
```

`Group.Run` takes an `errgroup.Group` and starts each item in it, so the
caller owns the wait and the first real failure cancels the rest. `Item.Run`
and `Item.Close` are both optional: a component that only needs stopping, such
as a cache or a connection pool, supplies `Close` alone.

Steps 1 and 2 are ordered for a reason: a configuration error has to be
reportable, and `logging.Bootstrap` exists precisely so there is a logger
before the configured one is built.

## Configuration

Defaults, help text and persistence rules live on the struct. See
[`config`](../config/README.md) for the full tag reference.

The one rule worth repeating here, because getting it wrong is a security bug
rather than a bug:

```go
type Config struct {
    Wallet struct {
        Password string `help:"wallet encryption password" secret:"true"`
    }
}
```

A `secret` field **may not carry a `default` or `releaseDefault`**; `Bind`
panics if it does. A default reaches production, so a placeholder password in
the source tree is the password of every deployment that never overrode it, and
it is in every clone, image and CI log. It is not a weak secret, it is a public
one.

Then call `CheckSecrets` at startup, or the rule only covers half the problem:
without a shipped placeholder *and* without a startup check, removing the
placeholder silently yields an empty key.

For a local convenience value, pair a `devDefault` with an explicitly empty
`releaseDefault`:

```go
Password string `secret:"true" devDefault:"dev-only" releaseDefault:""`
```

## Build stamp and metrics

A plain `go build` already gives you the commit, the build time and whether the
tree was dirty. Mount the debug server and `/version/` works with no wiring.

Two lines get the rest:

```go
monitor.Register(nil)   // process and runtime series, so /metrics is not empty
version.Register(nil)   // the version_info series
```

Only the semantic version needs a Makefile flag, because `go build` does not
read your git tag:

```makefile
VERSION_PKG := gitlab.internal/tuan.quang.tran/aioz-common/version
LDFLAGS := -X $(VERSION_PKG).buildVersion=$(shell git describe --tags --abbrev=0) \
           -X $(VERSION_PKG).channel=release
```

`--abbrev=0` matters: bare `git describe` yields `v2.4.1-3-gabc1234`, which
semver reads as a prerelease *of* v2.4.1, so a build three commits past the tag
sorts below it. `-X` also only patches a plain `string` variable - aimed at a
struct field it names a symbol that does not exist and the linker silently
drops it.

`channel` selects which config defaults apply (`devDefault` vs
`releaseDefault`). It is declared rather than derived, because it is a choice
about how the binary should behave. `Info.Release` is the opposite: it is
derived from evidence, so a build from a dirty tree reports `release: false`
however it was labelled.

## Choosing a credential

This is the decision people get wrong most often, so it gets a table.

| You need | Use | Why |
| --- | --- | --- |
| A session, CSRF token, invite, password reset | [`secret.NewToken`](../secret/README.md) | Opaque, hashed at rest, revoked with a `DELETE` |
| An API key a machine presents | `secret.NewKey` | Public id for O(1) lookup, secret half hashed, checksum for offline rejection |
| MFA recovery codes | `secret.NewCodes` | Crockford base32, folds what a person mistypes |
| A token a verifier checks **without reaching your database** | [`jwt`](../jwt/README.md) | Self-describing, signed |
| A database key or a correlation id | [`uuid`](../uuid/README.md) | Sortable, not a credential |
| A password | bcrypt or argon2id, in your service | Needs a slow, tunable hash; none of the above |

**Default to `secret`, not `jwt`.** A JWT cannot be revoked before it expires,
which is the price of the verifier not needing a lookup. If both ends already
talk to the same coordinator on every request, you are paying that price for
nothing. Reach for `jwt` when the verifier is a third party, an edge worker, or
another company's service.

`uuid.New()` is UUIDv7: about 74 random bits behind a 48-bit millisecond
timestamp. It is a database key and never a bearer token.

## Testing against these packages

Three of the packages exist mostly to make tests deterministic, and a service
that ignores them ends up with a slow, flaky suite:

- [`time2`](../time2/README.md) - take a `context.Context` and read the clock
  with `time2.Now(ctx)`. A test then advances a machine instead of sleeping.
  `jwt` does this, which is how its expiry tests run in microseconds.
- [`cycle`](../cycle/README.md) - `TriggerWait()` runs a background chore once,
  synchronously, so a test asserts on the result rather than on a timer.
- [`stats/testcontext`](../stats/README.md) - a context canceled when the test
  ends, with `Go`/`Wait`/`Cleanup` for background goroutines.

```go
ctx, machine := time2.WithNewMachine(context.Background())
machine.BlockThenAdvance(ctx, 1, time.Hour)   // wait for the timer, then jump
```

Use `BlockThenAdvance`, not bare `Advance`: if the code under test has not
created its timer yet, the jump passes it by and the test hangs or passes for
the wrong reason.

## Errors that cross the HTTP boundary

[`apierr`](../apierr/README.md) carries a status, a machine-readable reason
slug and the underlying cause. A service defines its own reasons on top rather
than inventing a parallel error type.

## Migrating from the old packages

| Old import | New import |
| --- | --- |
| `aioz-config` | `aioz-common/config` |
| `aioz-logger` | `aioz-common/logger` |
| `aioz-stats/...` | `aioz-common/stats/...` |
| `aioz-common/stats/version` | `aioz-common/version` |
| `aioz-template/internal/utils/...` | `aioz-common/{apierr,cycle,lifecycle,logging}` |

`stats/version` still works: it is aliases with deprecation notices. Point new
`-ldflags` at the new path, though, because the link-time variables live there.

For a service still on depin's copies of these utilities, the twelve packages
imported from depin kept their original package names, so the change is an
import path rewrite rather than an API rewrite:

```
aioz-depin/internal/memory   ->  aioz-common/memory
aioz-depin/pkg/common/time2  ->  aioz-common/time2
aioz-depin/pkg/readcloser    ->  aioz-common/readcloser
```

The exceptions are the packages that already existed here under a different
API - `lifecycle`, `debug`, `version`, `testcontext`, `cfgstruct` and
`grpcerr` - which need a real reconciliation rather than a rewrite.
