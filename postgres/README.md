# postgres

`pgxpool.New` does not connect. It returns a pool whose idle connections are
created by a goroutine that discards its error, and `MinConns` defaults to zero,
so with the default configuration nothing is dialed at all. A wrong host, an
expired password or a firewall in the way produces a process that starts
perfectly cleanly and reports itself healthy, and the failure surfaces on the
first business query, attributed to whatever code happened to make it. `Open`
pings, so a misconfigured database fails at startup next to the configuration
that caused it.

The second thing this prevents is a pool nobody can see into. The number that
predicts a database outage is not query latency - it is how long a caller waited
to be handed a connection, and nothing measures that by default.

```go
import (
    "github.com/jackc/pgx/v5"

    "gitlab.internal/tuan.quang.tran/aioz-common/postgres"
)
```

## Usage

```go
db, err := postgres.Open(ctx, log, cfg.Postgres)
if err != nil {
    return err
}
monkit.Default.ScopeNamed("postgres").Chain(db)
group.Add(lifecycle.Item{Name: "postgres", Close: db.Close})

var body string
err = db.QueryRow(ctx, `SELECT body FROM greetings WHERE id = $1`, id).Scan(&body)
switch {
case errors.Is(err, pgx.ErrNoRows):
    return apierr.NotFound.New("no greeting %d", id)
case err != nil:
    return err
}
```

`Chain` is the caller's call, not the constructor's: `Scope.Chain` appends
unconditionally, so a pool that registered itself would double-count every one a
test opened.

## `pgx.ErrNoRows` is an empty result, not a failure

It wraps `sql.ErrNoRows`, and a service that logs every `Scan` error at ERROR
pages itself on a lookup that found nothing.

The metrics cannot make that mistake, and for a reason worth knowing: `Rows.Close`
ends the query trace with a nil error, and `ErrNoRows` is synthesized by
`Scan` *afterwards*, so it never reaches the tracer at all. Unlike `redis.Nil`,
which go-redis hands straight to its hook, this one is only ever the caller's
problem.

## Session settings belong on the connection, not on the query

`search_path`, `application_name` and the two timeouts are sent in the startup
packet, via `RuntimeParams`. Three reasons, and the second is the important one:

- Startup parameters cost no round trip and are in force before the first query
  on a connection can observe anything else.
- **A `SET` on a pooled connection outlives the query that issued it.** The next
  borrower inherits it, so one caller setting `search_path` for a single lookup
  silently changes schema resolution for unrelated queries afterwards. In a
  schema-per-tenant service that is a cross-tenant data leak, not a performance
  note.
- Every connection in the pool is then identical, so a failure cannot depend on
  which one you happened to be given - the worst class of intermittent bug.

There is a test that acquires two connections at once and asserts both see the
same values, because that is the assertion that proves it is the pool and not
one connection.

## A transaction left open and idle is bounded; a slow query is not

`IdleInTransactionSessionTimeout` defaults to a minute.
`StatementTimeout` defaults to nothing.

The asymmetry is deliberate. A shared library cannot know a service's slowest
legitimate query, and a wrong statement timeout turns a working monthly report
into an error at month end. But there is no legitimate reason to hold a
transaction open and idle for a minute: that is a `defer tx.Rollback()` somebody
forgot, and it pins a connection *and* stops `VACUUM` reclaiming anything newer
than that transaction's snapshot, across the whole cluster. The default bounds a
bug, not a query.

## `sslmode` is the only TLS knob, and `prefer` is not encryption

There is no `TLSSkipVerify` field here, unlike [`redis`](../redis/README.md).
Two reasons:

- `sslmode` already spans the whole ladder, `require` included, which is what
  skip-verify would have been.
- Setting `ConnConfig.TLSConfig` after parsing - the way the redis package sets
  its options - would be a bug. `prefer` and `allow` are implemented as a
  *fallback list*, each entry carrying its own `*tls.Config`, so a post-parse
  assignment would configure the first attempt and silently leave the fallback
  alone.

libpq's default, `prefer`, is an unverified TLS attempt with a plaintext
fallback. Shipping that as a release default would ship an unencrypted database
connection to everyone who never set the flag, so the release default is
`require` and `prefer` is left for local development.

## A DSN is a credential

`URL` overrides the discrete fields entirely - never half of each - and the
`sslmode` flag is not merged into a DSN that already carries one, because
`sslmode` is part of where and how you connect, which is what a DSN is. A pool
size is not, so pool and timeout settings *are* applied on top.

pgx keeps the password out of `ParseConfigError.Error`, but
`ParseConfigError.ConnString` is an exported field holding the raw DSN: the
error string is safe while `%+v`, `slog.Any` and a JSON handler are not. This
package clears it. The connection log line names the host, port, database and
user, and never the DSN.

## Query metrics are tagged by pool, never by SQL

The histograms - `postgres_query`, `postgres_batch`, `postgres_copy_from`,
`postgres_connect`, `postgres_acquire` - carry the pool name and nothing else.

SQL text is unbounded by construction, and the prepared statement name is no
shortcut: pgx names them `stmtcache_<hex hash of the SQL>`, which is equally
unbounded and unreadable. Failure *events* are rare, so they can afford two more
tags: `code`, from a closed vocabulary of the five-character SQLSTATE plus
`canceled`, `deadline`, `conn` and `other`; and `op`, when the caller set one
with `WithOperation`. A SQLSTATE is the most useful label there is - `23505`
unique violation, `40001` serialization failure, `57014` statement timeout and
`53300` too many connections are four entirely different incidents that one
error counter cannot tell apart.

`postgres_acquire` is the series that does not exist anywhere else, and the
reason this package implements `pgxpool.AcquireTracer` at all.
`Stat().AcquireDuration` is a cumulative total, so it gives you a mean over all
time and no percentiles; the distribution of how long callers waited for a
connection is what predicts an outage.

**What this cannot do, honestly:** there is no per-operation latency histogram,
and there cannot be one at bounded cost. A service that wants per-repository
latency should wrap its own method in `mon.Task()` - one line, at a call site
where the name is a compile-time constant, and it includes the row scanning that
`postgres_query` excludes.

## What `postgres_query` actually measures

From the start of the query to the close of its `Rows`, not server time.
`TraceQueryEnd` fires from `Rows.Close`, so a caller that holds rows open while
doing other work inflates the number.

For `Exec` and `QueryRow(...).Scan(...)` the window is tight and means what you
expect. For a manual `for rows.Next()` loop it includes you - prefer
`pgx.CollectRows` and `pgx.ForEachRow`, which also close for you.

## Nothing lints an unclosed `pgx.Rows`

`sqlclosecheck` and `rowserrcheck` match `database/sql` types only, and `errcheck`
never fires because `pgx.Rows.Close` and `pgxpool.Pool.Close` return nothing. So
CI will not catch a leaked `Rows`, and a leaked `Rows` holds a connection.

The mitigation is the same one above: `pgx.CollectRows`, `pgx.CollectOneRow` and
`pgx.ForEachRow` close the rows and surface `rows.Err()` for you.

## A sort chosen by the request goes through an allowlist

`ORDER BY` cannot take a bind parameter, so a sort column from the query string
is where SQL injection survives in code that parameterizes everything else.
`SortColumns` maps the keys an endpoint documents to the columns they sort by:

```go
var mediaSort = postgres.SortColumns{
    "title":   {"media", "title"},
    "created": {"media", "created_at"},
}

orderBy, err := mediaSort.OrderBy(q.Get("sort"), q.Get("order"))
if errors.Is(err, postgres.ErrInvalidSort) {
    // 400
}
sql := "SELECT ... FROM media ORDER BY " + orderBy + ", media.id LIMIT $1 OFFSET $2"
```

- The key is matched **exactly**: no case folding, no trimming. A column name
  is not a key unless it is listed as one.
- The direction is `asc` or `desc` in any case; empty is `ASC`.
- The column is quoted as an identifier (`"media"."title"`), so even a strange
  name in the allowlist cannot end the clause.
- `ErrInvalidSort` is the request's fault (400). A key that maps to an empty
  identifier is a different error: the allowlist is wrong.

With `OFFSET` pagination, finish with a unique column like the id: rows that
tie on the sort column otherwise come back in any order, and a page repeats
or skips them. NULLs sort last for `ASC` and first for `DESC`.

## What this package does not do

**No migrations.** A runner would force a tool and a directory layout on every
adopter, force a deploy-time decision about migrating at boot rather than as a
job, and make this package own the distributed lock that migrating from every
replica needs. A migration tool takes a DSN, and `Config.ConnString()` produces
one, so adopting `goose`, `atlas`, `golang-migrate` or `tern` costs nothing.

**No retry.** pgxpool already redials on acquire and refills in the background.
Backoff inside `Open` would make `Open` block for an unbounded configured time,
which destroys the "fails at startup next to its configuration" property this
package exists for. If a database container starts alongside the service, set
`RequireOnStart: false` and let the readiness probe call `Health`.

**No query builder, ORM, repository or `DB` interface.** `DB` embeds
`*pgxpool.Pool`, so every query, batch, copy and transaction pgx offers is
reachable unchanged. A caller who wants `database/sql` calls
`stdlib.OpenDBFromPool(db.Pool)`.

## API

```go
type Config struct{ ... }

func (config Config) Validate() error
func (config Config) ConnString() string
func (config Config) PoolConfig() (*pgxpool.Config, error)

const (
	DefaultHealthTimeout     = 5 * time.Second
	DefaultMaxConnLifetime   = time.Hour
	DefaultMaxConnIdleTime   = 30 * time.Minute
	DefaultHealthCheckPeriod = time.Minute
)

type DB struct {
	*pgxpool.Pool
	// contains filtered or unexported fields
}

func Open(ctx context.Context, log *slog.Logger, config Config) (*DB, error)
func OpenWith(ctx context.Context, log *slog.Logger, poolConfig *pgxpool.Config, config Config) (*DB, error)

func (db *DB) Health(ctx context.Context) error
func (db *DB) Close() error
func (db *DB) Stats(cb func(key monkit.SeriesKey, field string, val float64))

func WithOperation(ctx context.Context, operation string) context.Context

type SortColumns map[string]pgx.Identifier

func (c SortColumns) OrderBy(key, direction string) (string, error)

var ErrInvalidSort = errors.New("invalid sort")
var Error = errs.Tag("postgres")
```

## Tests

The container tests run against a real `postgres:17-alpine`. They skip when
there is no docker daemon, which is why CI sets the variable that turns the skip
into a failure:

```bash
go test ./postgres/...                          # skips without docker
AIOZ_REQUIRE_DOCKER=1 go test -race ./postgres/...
```

The wait strategy requires the ready line **twice**, and that is not
superstition: the official image starts Postgres once to run `initdb` and the
entrypoint scripts, shuts it down, and starts it again. Waiting for the port or
the first ready line connects to the `initdb` instance and flakes when it is
stopped underneath the test.
