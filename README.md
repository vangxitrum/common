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
| [`aioz-common/logging`](logging/README.md) | `logging` | The config layer over `logger`: the `Config` that builds the process handler, plus level/format parsing and a pre-config `Bootstrap` logger. |
| [`aioz-common/apierr`](apierr/README.md) | `apierr` | Errors that cross the HTTP boundary: status, machine-readable reason slug and underlying cause, plus `Trace`/`Tags` for reaching the origin stack through a wrap chain. A service defines its own reasons on top. |
| [`aioz-common/lifecycle`](lifecycle/README.md) | `lifecycle` | Starts a service's components together and closes them in reverse order, with named goroutines, pprof labels and a shutdown-timeout stack dump. |
| [`aioz-common/version`](version/README.md) | `version` | The build stamp, read from the toolchain's embedded VCS info so no Makefile wiring is needed: a comparable `SemVer`, a derived dev version that sorts below every release, the dev/release defaults channel, and a `version_info` metrics series. |
| [`aioz-common/secret`](secret/README.md) | `secret` | Bearer secrets that are never at rest in a replayable form: `Token` redacts itself in fmt, slog, JSON and SQL and refuses to be stored, `Hash` is what you keep. Prefixed API keys with an embedded lookup id, and recovery codes that fold what a person mistypes. |
| [`aioz-common/uuid`](uuid/README.md) | `uuid` | Identifiers anything can generate without coordinating - no database round trip, no collisions across machines. A `[16]byte`, so it is comparable and works as a map key; version 7, so ids sort by creation time. |
| [`aioz-common/currency`](currency/README.md) | `currency` | Money as a whole number of tiny units, so it never touches a float. `USD` and `AIOZ` count base units at 18 decimals and cannot be added to each other, because the currency is part of the type; `Decimal` is the rate they are computed from. |
| [`aioz-common/cycle`](cycle/README.md) | `cycle` | Runs a function on an interval, with `Trigger`/`TriggerWait` so a background chore is testable without sleeping. |
| [`aioz-common/time2`](time2/README.md) | `time2` | A `Clock` carried on the context, so code under test advances time instead of spending it; `Machine` drives timers and tickers deterministically. |
| [`aioz-common/memory`](memory/README.md) | `memory` | A byte count that carries its unit. Parses and prints both the base-2 and base-10 families, binds to a flag, and survives YAML and JSON as a string rather than an unlabeled number. |
| [`aioz-common/date`](date/README.md) | `date` | Month and day boundaries in UTC, and the arithmetic between them, so every billing and reporting query agrees on where a month starts. |
| [`aioz-common/period`](period/README.md) | `period` | A calendar month as a comparable value, with the half-open time range it stands for. The `YYYY-MM` format is validated once, at the edge. |
| [`aioz-common/lrucache`](lrucache/README.md) | `lrucache` | An LRU with a TTL that fills a miss once per key however many callers missed together, so an expiring hot key does not stampede the backend. |
| [`aioz-common/errs2`](errs2/README.md) | `errs2` | Tells a cancellation apart from a fault, and a `Group` that returns every goroutine's error rather than only the first. |
| [`aioz-common/signal`](signal/README.md) | `signal` | A one-shot error others can wait on or select on, and a `chan struct{}` that is not allocated until something uses it. |
| [`aioz-common/context2`](context2/README.md) | `context2` | The three context shapes the standard library lacks: work that outlives its request, a deadline set after the fact, and a cancellation that carries a reason. |
| [`aioz-common/slices2`](slices2/README.md) | `slices2` | `Map` over a slice, plus the two fallible forms: stop at the first error, or report every one. |
| [`aioz-common/readcloser`](readcloser/README.md) | `readcloser` | `io.Reader` combinators that also close: multi, lazy, limit and always-fails, none of which the standard library offers as a `ReadCloser`. |
| [`aioz-common/leak`](leak/README.md) | `leak` | Records the stack where a resource was created and reports the ones nobody closed, so a leak names its opener. Tracking is a `-race`-only cost. |
| [`aioz-common/base58`](base58/README.md) | `base58` | base58 and base58check, vendored from btcd. |
| [`aioz-common/stats/debug`](stats/README.md) | `debug` | Single HTTP debug server: monkit metrics in Prometheus format, pprof, request-frequency `/top`, `/version/`, `/health`, dynamic log level. |
| [`aioz-common/stats/monitor`](stats/monitor/README.md) | `monitor` | Registers process and runtime series (goroutines, memory, GC, rusage, fds, uptime) into a monkit registry, so `/metrics` is not empty before the first request. |
| [`aioz-common/stats/testcontext`](stats/README.md) | `testcontext` | Test helper: a context canceled when the test ends, with `Go`/`Wait`/`Cleanup` for tracking background goroutines and reporting their errors against the test. |

Each component keeps its own README with the full API reference and examples -
follow the links above.

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
├── errs2/            # cancellation, and error groups
├── leak/             # resource-leak tracking
├── lifecycle/        # component start/stop
├── logger/           # the slog.Handler
├── logging/          # its configuration
├── lrucache/         # LRU + TTL + single-flight fill
├── memory/           # byte counts with units
├── period/           # a calendar month as a value
├── readcloser/       # io.ReadCloser combinators
├── secret/           # bearer secrets and their hashes
├── signal/           # one-shot errors, lazy channels
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
make test
```

CI runs `lint` then `test` on merge requests and on the default branch; lint is
blocking.

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
