# Changelog

## v0.3.0 (2026-09-10)

Release note: [`.gitlab/releases/v0.3.0.md`](.gitlab/releases/v0.3.0.md).

Twelve utility packages imported from `depin`, eight new packages, and one
security rule added to `config`. The new ones cover the three dependencies every
service actually has - Postgres, outbound HTTP, and the backoff between
attempts at either. Everything is additive except the move of
`stats/version`, which leaves aliases behind.

### Added

**Twelve packages imported from depin**, with their commit history, keeping
their original package names so a later migration off depin's copies is an
import path rewrite rather than an API rewrite:

`base58`, `context2`, `date`, `errs2`, `leak`, `lrucache`, `memory`, `period`,
`readcloser`, `signal`, `slices2`, `time2`.

**`secret`** - bearer secrets that are never at rest in a replayable form.
`Token` is plaintext and redacts itself through fmt, slog, JSON and SQL;
`Hash` is what you store. Prefixed API keys carry a public lookup id, so
verification is one indexed row rather than a scan over every stored hash.
Recovery codes use Crockford base32, which folds the characters people
mistype. See [`secret/README.md`](secret/README.md).

**`jwt`** - signed tokens for a verifier that cannot reach the issuer's
database, with a shared secret or a key pair. The algorithm is pinned at
construction and the header's `alg` is never trusted, closing algorithm
confusion and `alg: none`. An expiry is required on both sides. `Ring`
rotates keys on `kid` without invalidating tokens in flight. See
[`jwt/README.md`](jwt/README.md).

**`version.SemVer`** - a comparable semantic version, tolerant on input
because internal builds disagree about the leading `v`. An untagged build now
derives `v0.0.0-dev.<unix>.g<commit>`, which sorts below every real tag.

**`version.Register`** - the build stamp as a monkit `StatSource`, so
`/metrics` carries a `version_info` series.

**`config` secret handling** - `secret:"true"` / `secret:"optional"` on a
field, and `config.CheckSecrets` at startup. See below.

**`redis`** - a configured, observable go-redis client. `Open` dials and pings,
so an address that is wrong, a password that expired or a firewall in the way
fails at startup next to the configuration that caused it rather than on the
first business request. The pool carries a monkit hook that counts a
`redis.Nil` as a miss instead of an error, timeouts and pool limits are config
fields, and `Client` embeds `*redis.Client` so no command is wrapped. Across
the estate today, three options are ever set, nothing configures a timeout,
nothing has a redis metric and nothing calls `Close`. See
[`redis/README.md`](redis/README.md).

**`rabbitmq`** - the AMQP client, replacing eight hand-rolled wrappers, three
of them stale forks of each other. `Conn` supervises a connection with
exponential backoff and reconnects indefinitely; `Topology` is re-declared on
every reconnect; `Publisher` correlates every confirm to its own delivery tag;
`Consumer` runs a bounded worker pool with a dead-letter policy and a drain
that loses nothing at shutdown. Three live bugs in the wrappers it replaces -
stolen publisher confirms, a self-deadlocking close, and a reconnect loop
watching a dead channel - cannot be written against this API. See
[`rabbitmq/README.md`](rabbitmq/README.md).

**`retry`** - exponential backoff with full jitter. The three retry loops in
`rabbitmq` were each written by hand and none of them waits a random amount of
time, so when a broker restarts every instance that was connected to it fails in
the same instant, waits the same second, and reconnects in the same instant -
and stays synchronized through every doubling. `Do` covers bounded work; `Loop`
covers a supervisor that starts its wait over after each success. What is worth
retrying is a classifier, so a bug is not retried four times; whether the caller
has given up is read from the context and never from the error, because an inner
per-attempt timeout and an outer deadline are the same
`context.DeadlineExceeded` and only the context tells them apart. The clock
comes from `time2`, so the tests advance time rather than spend it. The
`rabbitmq` loops are not yet migrated: full jitter halves their mean reconnect
wait, which is a behavior change that deserves its own. See
[`retry/README.md`](retry/README.md).

**`httpclient`** - the outbound HTTP client. `http.DefaultClient` has no
timeout, so a dependency that accepts a connection and never answers holds a
goroutine and a connection for the life of the process; `http.DefaultTransport`
keeps two idle connections per host, so at any real concurrency there is a fresh
TLS handshake on nearly every request. This is an `*http.Client` with both
fixed, plus per-request metrics whose tag set is bounded and carries no URL, a
redirect policy that refuses an https-to-http downgrade, a response body cap
that reports rather than truncating silently, and a User-Agent from the build
stamp. It is pure `net/http` - no server, no router, no framework - so it drops
into any vendored SDK that takes an `*http.Client`. Retry is a `RoundTripper`
and is **off by default**: a client that retries by default triples the load on
a dependency that is already failing, and a replayed POST corrupts data. A call
site opts in with `httpclient.WithRetry(ctx)`. See
[`httpclient/README.md`](httpclient/README.md).

**`httpclient` private-network guard** - `NewOneShot`, the client for URLs
someone else supplied, refuses to connect to loopback, private, link-local and
other internal addresses, the cloud metadata endpoint among them, which closes
server-side request forgery through webhooks and callbacks. The check is on the
resolved address at dial time, so DNS rebinding and redirects into the network
are refused too, and a refusal wraps `ErrBlockedAddress` and is never retried.
`New` allows internal addresses unless `private-networks: block` is set;
`allowed-networks` exempts prefixes. A guarded transport ignores `HTTP_PROXY`.

**`postgres`** - a configured, observable pgx connection pool. `pgxpool.New`
returns a pool that has not connected to anything, and its idle-connection
priming is a goroutine with a discarded error, so a wrong host or an expired
password produces a process that starts cleanly and fails on the first query;
`Open` pings, so it fails next to the configuration that caused it.
`search_path`, `application_name` and the statement timeouts are sent in the
startup packet rather than issued per query, which is what stops one caller's
`SET` from leaking into the next borrower of a pooled connection. A pgx tracer
records query, batch and - the number that actually predicts a database outage -
connection-acquire latency, tagged by pool and never by SQL text; failures carry
the SQLSTATE. `DB` embeds `*pgxpool.Pool`, so no query method is wrapped and
`stdlib.OpenDBFromPool` is there for anyone who wants `database/sql`. Migrations
are deliberately out of scope. See [`postgres/README.md`](postgres/README.md).

**`uuid.UUID.Value` now writes the canonical string, not the raw bytes.** The
bytes were cheaper for a `uuid` column, which is what the column holds, and
wrong for every other one: sixteen arbitrary bytes are not valid UTF-8, so a
`text` or `varchar` column rejected the whole statement with `invalid byte
sequence for encoding "UTF8"`, and `lib/pq` refuses a `[]byte` for a `uuid`
column outright. `Value` cannot know which kind of column it is being sent to,
so it now sends the form that works for all of them. `Scan` still accepts both.
This changes what new rows contain in a `bytea` column, which nothing here
writes; a `uuid` column is unaffected, since both forms produce the same value.

**`period.Period` and `version.SemVer` can be stored.** Both are structs, and
`database/sql` has no converter for a struct, so both were unusable as columns -
a failure that arrived at the first query rather than at compile time. They now
implement `driver.Valuer` and `sql.Scanner` over their string forms, and their
zero values store as `NULL`: "no period" and "no version" are real states, and a
column that wrote them as `0000-00` or `v0.0.0` could not tell them from real
ones. `memory.Size` needed nothing - its underlying type is `int64` - and there
is now a test against a real column that says so.

**`version.Info.UserAgent`** - the build stamp as a User-Agent product token, so
every outbound integration identifies itself the same way. The commit hash is
deliberately not in it: a per-commit User-Agent fragments whatever the remote
counts by user-agent into noise.

**`stats/testcontext`** gained `Go`, `Wait` and `Cleanup` for tracking
background goroutines in a test.

### Changed

**`stats/version` moved to `version`.** It stopped being a stats concern once
it grew version comparison and a metrics source. `stats/version` remains as
type aliases with deprecation notices, so no import breaks.

**`Info.Version` is now a `SemVer`, not a `string`.** Rendering is unchanged -
it still marshals as a `v`-prefixed string - but it can now be compared. Code
that treated the field as a string needs `.String()`.

**A `secret`-tagged config field may no longer carry a `default` or
`releaseDefault`.** `Bind` panics if it does. This is a deliberate breaking
change for any struct that shipped a placeholder secret, which is the point:
a default reaches production, so a placeholder password in the source tree is
the password of every deployment that never overrode it.

To keep a local convenience value, pair `devDefault` with an explicitly empty
`releaseDefault`:

```go
Password string `secret:"true" devDefault:"dev-only" releaseDefault:""`
```

### Fixed

`stats/README.md` documented `-ldflags -X` targeting struct fields
(`...version.Build.Release=`). The linker only patches plain `string`
variables, so those flags were silently doing nothing. The corrected
invocation targets package-level variables in the new `version` package, and
uses `git describe --abbrev=0`, since bare `git describe` output sorts below
its own tag under semver rules.

### Dependencies

Three modules added, each with no dependencies of its own:
`blang/semver/v4`, `golang-jwt/jwt/v5`, and `zeebo/errs/v2` promoted to
direct use. `zeebo/errs` v1, which arrived with the imported packages, was
converted away rather than added.

The two client packages add two build dependencies and one test dependency:

| Module | Version | Build-graph cost |
| --- | --- | --- |
| `github.com/rabbitmq/amqp091-go` | v1.14.0 | none. Its one requirement is its own test dependency and is pruned out of the build list. |
| `github.com/redis/go-redis/v9` | v9.22.0 | small: `cespare/xxhash/v2`, `go.uber.org/atomic`, and `golang.org/x/sys`, which was already here. |
| `github.com/testcontainers/testcontainers-go` | v0.40.0 | heavy, and test-only: the Docker SDK, containerd, OpenTelemetry, roughly fifty modules. |

testcontainers is the one worth arguing, and it is acceptable on two grounds.
A consumer never sees it: with module-graph pruning, a test-only dependency of
an imported package does not enter the importing module's build list, `go.sum`
or `go mod tidy` output. That is a different bill from the one `base58` was
vendored to avoid, which was a build dependency. And there is no alternative
for RabbitMQ: no credible fake broker exists, `miniredis` models no pool, no
pipelines and none of the failure modes that matter, and the value of both
packages is reconnect and redelivery behaviour that only a real server
restart exercises.

### Migration

See [`docs/ADOPTING.md`](docs/ADOPTING.md).

| Old | New |
| --- | --- |
| `aioz-common/stats/version` | `aioz-common/version` |
| `info.Version` (string) | `info.Version.String()` |
| `-X .../stats/version.Build.X=` | `-X .../version.buildVersion=` |
| `httpclient.NewOneShot` to an internal address | add its prefix to `allowed-networks`, or set `private-networks: allow` |

## v0.2.0

`uuid` and `currency`; the build stamp derived from Go build info; `apierr`
trimmed to its core.

## v0.1.0

The initial module, assembled from `aioz-config`, `aioz-logger`, `aioz-stats`
and `aioz-template`'s `internal/utils`.
