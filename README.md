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
| [`aioz-common/stats/debug`](stats/README.md) | `debug` | Single HTTP debug server: monkit metrics in Prometheus format, pprof, request-frequency `/top`, `/version/`, `/health`, dynamic log level. |
| [`aioz-common/stats/version`](stats/README.md) | `version` | Build metadata (release, SHA, timestamp) injected via `-ldflags -X`, served by the debug server. |
| [`aioz-common/stats/testcontext`](stats/README.md) | `testcontext` | Test helper: a context with goroutine tracking and deadline handling. |

Each component keeps its own README with the full API reference and examples -
follow the links above.

## Layout

```
aioz-common/
├── config/           # package config
├── logger/           # package logger
└── stats/            # observability
    ├── debug/        # package debug
    ├── version/      # package version
    └── testcontext/  # package testcontext
```

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

The originals are archived and should no longer be imported.
