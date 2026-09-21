# aioz-common

Shared Go libraries for AIOZ backend services. One module, one release cycle,
one CI pipeline - consumers add a single `require` line instead of tracking
three separate repositories.

```bash
go get gitlab.internal/tuan.quang.tran/aioz-common
```

## Components

| Import path | Package | What it does |
| --- | --- | --- |
| [`aioz-common/config`](config/README.md) | `config` | Struct-tag driven configuration: defaults, help text and persistence rules on the struct; binds to flags, loads YAML + environment, generates a documented default config file. |
| [`aioz-common/logger`](logger/README.md) | `logger` | A `slog.Handler` adding async dispatch, buffered/rotating file output, Graylog (GELF) shipping and per-level sampling on top of stdlib `log/slog`. |
| [`aioz-common/logger/logtest`](logger/logtest/README.md) | `logtest` | Loggers for tests: one writing into `t.Log`, and a `Recorder` that keeps every record - message, level, flattened attributes, errors as errors - for assertions. |
| [`aioz-common/logging`](logging/README.md) | `logging` | The config layer over `logger`: the `Config` that builds the process handler, plus level/format parsing and a pre-config `Bootstrap` logger. |
| [`aioz-common/apierr`](apierr/README.md) | `apierr` | Errors that cross the HTTP boundary: status, machine-readable reason slug and underlying cause, plus `Trace`/`Tags` for reaching the origin stack through a wrap chain. A service defines its own reasons on top. |
| [`aioz-common/lifecycle`](lifecycle/README.md) | `lifecycle` | Starts a service's components together and closes them in reverse order, with named goroutines, pprof labels and a shutdown-timeout stack dump. |
| [`aioz-common/version`](version/README.md) | `version` | The build stamp, read from the toolchain's embedded VCS info so no Makefile wiring is needed: a comparable `SemVer`, a derived dev version that sorts below every release, the dev/release defaults channel, and a `version_info` metrics series. |
| [`aioz-common/jwt`](jwt/README.md) | `jwt` | Signed tokens for a verifier that cannot reach the issuer's database, with a shared secret or a key pair. The algorithm is pinned at construction and the header's `alg` is never trusted, so algorithm confusion and `none` are closed; an expiry is required; a `Ring` rotates keys without invalidating tokens in flight. |
| [`aioz-common/secret`](secret/README.md) | `secret` | Bearer secrets that are never at rest in a replayable form: `Token` redacts itself in fmt, slog, JSON and SQL and refuses to be stored, `Hash` is what you keep. Prefixed API keys with an embedded lookup id, and recovery codes that fold what a person mistypes. |
| [`aioz-common/uuid`](uuid/README.md) | `uuid` | Identifiers anything can generate without coordinating - no database round trip, no collisions across machines. A `[16]byte`, so it is comparable and works as a map key; version 7, so ids sort by creation time. |
| [`aioz-common/currency`](currency/README.md) | `currency` | Money as a whole number of tiny units, so it never touches a float. `USD` and `AIOZ` count base units at 18 decimals and cannot be added to each other, because the currency is part of the type; `Decimal` is the rate they are computed from. |
| [`aioz-common/cycle`](cycle/README.md) | `cycle` | Runs a function on an interval, with `Trigger`/`TriggerWait` so a background chore is testable without sleeping. |
| [`aioz-common/retry`](retry/README.md) | `retry` | Exponential backoff with full jitter, so a dependency that restarts is not met by every instance at once. A classifier decides what is worth retrying, the context decides when to stop, and the clock comes from `time2`, so a test never sleeps. |
| [`aioz-common/time2`](time2/README.md) | `time2` | A `Clock` carried on the context, so code under test advances time instead of spending it; `Machine` drives timers and tickers deterministically. |
| [`aioz-common/memory`](memory/README.md) | `memory` | A byte count that carries its unit. Parses and prints both the base-2 and base-10 families, binds to a flag, and survives YAML and JSON as a string rather than an unlabeled number. |
| [`aioz-common/date`](date/README.md) | `date` | Month and day boundaries in UTC, and the arithmetic between them, so every billing and reporting query agrees on where a month starts. |
| [`aioz-common/period`](period/README.md) | `period` | A calendar month as a comparable value, with the half-open time range it stands for. The `YYYY-MM` format is validated once, at the edge. |
| [`aioz-common/lrucache`](lrucache/README.md) | `lrucache` | An LRU with a TTL that fills a miss once per key however many callers missed together, so an expiring hot key does not stampede the backend. |
| [`aioz-common/redis`](redis/README.md) | `redis` | A configured, observable go-redis client: it dials and pings at startup so a bad address fails next to its configuration, carries timeouts, pool limits and a metrics hook that knows a miss from an error, and embeds the library's own client so no command is wrapped. |
| [`aioz-common/rabbitmq`](rabbitmq/README.md) | `rabbitmq` | The AMQP client: a supervised connection with exponential backoff, a topology re-declared on every reconnect, a publisher with per-publish confirms, and a consumer with a bounded worker pool, a dead-letter policy and a graceful drain. |
| [`aioz-common/postgres`](postgres/README.md) | `postgres` | A configured, observable pgx pool: it pings at startup because `pgxpool.New` returns a pool that has not connected to anything, sends session settings once per connection rather than per query, measures how long callers wait for a connection, and embeds `*pgxpool.Pool` so no query method is wrapped. |
| [`aioz-common/httpclient`](httpclient/README.md) | `httpclient` | The outbound `*http.Client` with the timeouts, connection pool, redirect policy and metrics `http.DefaultClient` has none of. Pure `net/http`: no server, no router, no framework. Retry is opt-in per call site. |
| [`aioz-common/depin`](depin/README.md) | `depin` | Files stored and fetched through the depin network, behind a provider-neutral `Storage`. The go-sdk is wrapped rather than used directly: its identifiers live in an internal package no other module may import, it logs through zap, and it reports every failure as a formatted string. So ids cross as `uuid.UUID`, its logs are bridged into the caller's `slog.Logger`, and a failure says whether trying again could help. Needs a `replace` directive; see its README. |
| [`aioz-common/slack`](slack/README.md) | `slack` | Messages posted to Slack through a provider-neutral `Notifier`. The Web API answers 200 with `{"ok": false}` for most failures, so the body decides and the status only settles the answers that carry no body. A closed set of error codes says whether trying again could help, a `Retry-After` is honored up to a bound, and the bot token is a `secret.Token`. |
| [`aioz-common/s3client`](s3client/README.md) | `s3client` | Objects stored in an S3-compatible bucket, ours included. Our own service rejects the flexible checksums aws-sdk-go-v2 now sends by default, so the workarounds are the package: checksums only where required, on the client *and* on the uploader, which does not inherit the client's setting; path style forced; and `DeleteObjects` signed with a `Content-MD5` from a middleware swapped in before SigV4 rather than added after. The uploader is built once, a missing object is a code rather than a false, and a body comes back as an `io.ReadCloser`. |
| [`aioz-common/clientip`](clientip/README.md) | `clientip` | The address a request really came from, behind reverse proxies: `X-Forwarded-For` walked from the right past a trusted-proxy list, so a client cannot pick the address it is rate limited and logged by. |
| [`aioz-common/ratelimit`](ratelimit/README.md) | `ratelimit` | Rate limits shared by every replica: GCRA windows in Redis with an exact Retry-After, a denial that consumes nothing so no one can be locked out on purpose, hashed keys, and a bounded fallback when Redis fails. |
| [`aioz-common/password`](password/README.md) | `password` | Password storage to OWASP and NIST: argon2id at OWASP's floor, one derivation on every path so response time does not reveal which accounts exist, legacy bcrypt upgraded on login, NFC, and a bound on how many hashes hold memory at once. |
| [`aioz-common/httpsec`](httpsec/README.md) | `httpsec` | The OWASP response headers (CSP, HSTS, nosniff, framing, referrer, COOP/CORP, no-store) and a CORS policy that is off until configured and refuses the configurations that leak data. Framework-free: `http.Header` in, with gin and echo adapters in the README. |
| [`aioz-common/upload`](upload/README.md) | `upload` | Form-file checks from OWASP's File Upload cheat sheet: the extension and the sniffed content must agree on an allowed type, the client's `Content-Type` is ignored, the size fails rather than truncates, and the client's filename is only ever a sanitized display name. |
| [`aioz-common/errs2`](errs2/README.md) | `errs2` | Tells a cancellation apart from a fault, and a `Group` that returns every goroutine's error rather than only the first. |
| [`aioz-common/signal`](signal/README.md) | `signal` | A one-shot error others can wait on or select on, and a `chan struct{}` that is not allocated until something uses it. |
| [`aioz-common/context2`](context2/README.md) | `context2` | The three context shapes the standard library lacks: work that outlives its request, a deadline set after the fact, and a cancellation that carries a reason. |
| [`aioz-common/slices2`](slices2/README.md) | `slices2` | `Map` over a slice, plus the two fallible forms: stop at the first error, or report every one. |
| [`aioz-common/readcloser`](readcloser/README.md) | `readcloser` | `io.Reader` combinators that also close: multi, lazy, limit and always-fails, none of which the standard library offers as a `ReadCloser`. |
| [`aioz-common/leak`](leak/README.md) | `leak` | Records the stack where a resource was created and reports the ones nobody closed, so a leak names its opener. Tracking is a `-race`-only cost. |
| [`aioz-common/base58`](base58/README.md) | `base58` | base58 and base58check, vendored from btcd. |
| [`aioz-common/stats/debug`](stats/README.md) | `debug` | Single HTTP debug server: monkit metrics in Prometheus format, pprof, request-frequency `/top`, `/version/`, `/health`, dynamic log level. |
| [`aioz-common/stats/monitor`](stats/monitor/README.md) | `monitor` | Registers process and runtime series (goroutines, memory, GC, rusage, fds, uptime) into a monkit registry, so `/metrics` is not empty before the first request. |
| [`aioz-common/stats/testcontext`](stats/testcontext/README.md) | `testcontext` | Test helper: a context canceled when the test ends, with `Go`/`Wait`/`Cleanup` for tracking background goroutines and reporting their errors against the test. |
| [`aioz-common/stats/version`](stats/version/README.md) | `version` | **Deprecated.** Aliases forwarding to [`version`](version/README.md), which is where the package now lives. |

Each component keeps its own README with the full API reference and examples -
follow the links above. [`docs/ADOPTING.md`](docs/ADOPTING.md) covers what a
service wires up on day one and how the packages fit together;
[`CHANGELOG.md`](CHANGELOG.md) records what changed between versions.

## Layout

```
aioz-common/
├── apierr/           # HTTP error envelope
├── base58/           # base58 and base58check
├── config/           # configuration
├── context2/         # the context shapes stdlib lacks
├── currency/         # exact money and rates
├── cycle/            # interval runner
├── date/             # UTC month and day boundaries
├── depin/            # depin network storage client
│   └── depintest/    # a recording Storage for tests
├── errs2/            # cancellation, and error groups
├── httpclient/       # the outbound HTTP client
├── leak/             # resource-leak tracking
├── lifecycle/        # component start/stop
├── logger/           # the slog.Handler
├── jwt/              # signed tokens, algorithm pinned
├── logging/          # its configuration
├── lrucache/         # LRU + TTL + single-flight fill
├── memory/           # byte counts with units
├── period/           # a calendar month as a value
├── postgres/         # configured, observable pgx pool
├── rabbitmq/         # AMQP connection, topology, publisher, consumer
├── readcloser/       # io.ReadCloser combinators
├── redis/            # configured, observable go-redis client
├── retry/            # exponential backoff with jitter
├── s3client/         # S3-compatible object storage
├── secret/           # bearer secrets and their hashes
├── signal/           # one-shot errors, lazy channels
├── slack/            # Slack Web API notifier
│   └── slacktest/    # a recording Notifier for tests
├── slices2/          # Map and its fallible forms
├── time2/            # testable clocks and timers
├── uuid/             # identifiers
├── version/          # the build stamp and semantic versions
└── stats/            # observability
    ├── debug/        # the debug HTTP server
    ├── monitor/      # process and runtime series
    ├── version/      # deprecated alias for version/
    └── testcontext/  # test helper
```

`logging` is deliberately separate from `logger` rather than folded into it:
both declare `Config`, `FileConfig` and `New`, so merging them is an API
rewrite, not a move. `logger` is the handler; `logging` is what configures it.

`redis`, `rabbitmq` and `postgres` are the packages here that wrap someone else's
client, and they stop at different lines, on purpose. go-redis already handles
reconnection and pooling, so `redis` adds construction, a startup ping, metrics
and `Close`, hands back the library's own type, and wraps not one command.
`amqp091` handles nothing above the wire, so `rabbitmq` owns the connection
supervisor, topology, publisher and consumer that seven services had each
written for themselves. Neither defines a Cache or Queue interface: the moment
there is one, every feature the underlying library grows has to be re-exported
through it, and services start choosing between the abstraction and the thing
it abstracts. `postgres` stops in the same place `redis` does, for a different
failure: go-redis dials lazily and fails on the first command, while `pgxpool`
never dials at all and fails on the first acquire.

`httpclient` is a client and nothing else. There is no server, router or
middleware package here and there will not be one: the services do not share an
HTTP framework, so anything tied to one is unusable by half of them. What they
do share is the outbound side, because `*http.Client` and `http.RoundTripper`
are stdlib types every framework and every vendored SDK already accepts.

`testcontext` lives under `stats/` because that is where it was authored (in
the former `aioz-stats` repository) and moving it would break import paths for
no functional gain. `version` did move out, once it grew version comparison and
a metrics source and stopped being a stats concern at all; `stats/version`
remains as a deprecated alias so nothing breaks.

## Development

```bash
make lint      # golangci-lint, same version pin as CI
make lint-fix
make fmt
make test              # unit tests, no docker
make test-integration  # the container tests, skipping turned off
```

CI runs `lint` then `test` on merge requests and on the default branch; lint is
blocking. A third `integration` stage runs the `redis`, `rabbitmq` and `postgres` container
tests against real servers, on the runner's own docker daemon through its
mounted socket (the runner is not privileged, so docker-in-docker cannot start).

The container tests are ordinary test files with no build tag, so they are
always compiled and always linted; they skip when there is no docker daemon.
`AIOZ_REQUIRE_DOCKER=1` turns that skip into a failure, which is how CI makes
sure the job cannot quietly stop testing anything.

## History

This module was assembled from three repositories, whose commit histories are
preserved here:

- `aioz-config` → `config/`
- `aioz-logger` → `logger/`
- `aioz-stats`  → `stats/`

`apierr`, `cycle`, `lifecycle`, `logging`, `stats/monitor` and the link-time
half of `stats/version` were then moved out of `aioz-template`'s
`internal/utils/`, their histories carried over with `git subtree split`.

`secret` is the one package here that was written for this module rather than
moved into it.

`base58`, `context2`, `date`, `errs2`, `leak`, `lrucache`, `memory`, `period`,
`readcloser`, `signal`, `slices2` and `time2` came from `depin` the same way,
out of its `internal/` and `pkg/common/` trees. They kept their original
package names, `2` suffixes included, so a service migrating off the `depin`
copies rewrites an import path rather than a call.

The original library repositories are archived and should no longer be
imported.

Note the consequence of importing this way. `git blame` resolves to the
original commits, so the line-level history is intact. `git log -- <path>`
stops at the import commit, because the file's pre-import path was different
(`internal/memory/size.go`, not `memory/size.go`) and path-limited log does not
follow it across the merge. To read a package's earlier history, walk the
import commit's second parent:

```bash
git log $(git rev-list --parents -n1 <import-commit> | awk '{print $3}')
```
