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
| [`aioz-common/uuid`](uuid/README.md) | `uuid` | A `[16]byte` UUID (version 7, so ids sort by creation time): comparable, usable as a map key, immutable once held, with SQL, JSON and gogo/protobuf handling. |
| [`aioz-common/currency`](currency/README.md) | `currency` | Exact money. `USD` and `AIOZ` as integer counts of base units, which cannot be added to each other because the currency is part of the type, plus the `Decimal` used to compute them. |
| [`aioz-common/cycle`](cycle/README.md) | `cycle` | Runs a function on an interval, with `Trigger`/`TriggerWait` so a background chore is testable without sleeping. |
| [`aioz-common/stats/debug`](stats/README.md) | `debug` | Single HTTP debug server: monkit metrics in Prometheus format, pprof, request-frequency `/top`, `/version/`, `/health`, dynamic log level. |
| [`aioz-common/stats/monitor`](stats/monitor/README.md) | `monitor` | Registers process and runtime series (goroutines, memory, GC, rusage, fds, uptime) into a monkit registry, so `/metrics` is not empty before the first request. |
| [`aioz-common/stats/version`](stats/version/README.md) | `version` | The build stamp, read from the toolchain's embedded VCS info so no Makefile wiring is needed, plus the dev/release defaults channel. |
| [`aioz-common/stats/testcontext`](stats/README.md) | `testcontext` | Test helper: a context with goroutine tracking and deadline handling. |

Each component keeps its own README with the full API reference and examples -
follow the links above.

## Layout

```
aioz-common/
├── apierr/           # HTTP error envelope
├── config/           # configuration
├── currency/         # exact money and rates
├── cycle/            # interval runner
├── lifecycle/        # component start/stop
├── logger/           # the slog.Handler
├── logging/          # its configuration
├── uuid/             # identifiers
└── stats/            # observability
    ├── debug/        # the debug HTTP server
    ├── monitor/      # process and runtime series
    ├── version/      # the build stamp
    └── testcontext/  # test helper
```

`logging` is deliberately separate from `logger` rather than folded into it:
both declare `Config`, `FileConfig` and `New`, so merging them is an API
rewrite, not a move. `logger` is the handler; `logging` is what configures it.

`version` and `testcontext` live under `stats/` because that is where they were
authored (in the former `aioz-stats` repository) and moving them would break
import paths for no functional gain.

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

The original library repositories are archived and should no longer be
imported.
