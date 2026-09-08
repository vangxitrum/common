# logging

Builds the process logger from its own configuration. This is the config layer
over [`logger`](../logger/README.md): `logger` is the `slog.Handler`, `logging`
is the `Config` that constructs one and the parsing that validates it.

The `Config` lives here, next to the code that consumes it, rather than in a
central config package: a package owns its settings, and the root `Config`
composes them.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/logging"
```

## Usage

```go
log := logging.Bootstrap()   // stderr, text, info - for use before config loads

handler, levelVar, err := logging.New(cfg.Logging)
if err != nil {
    return err
}
slog.SetDefault(slog.New(handler))
defer handler.Shutdown(ctx)
```

`levelVar` is a live `*slog.LevelVar`, so the level can be retuned on a running
process - that is what the debug server's `/logging` endpoint writes to.

## Configuration

The struct tags are read by [`config`](../config/README.md), so these become
flags, YAML keys and generated documentation without further wiring. The `log`
prefix below is the name the embedding struct gives the field, not something
this package chooses.

| key | default | meaning |
| --- | --- | --- |
| `log.level` | `info` (`debug` in dev) | minimum level: debug, info, warn, error |
| `log.format` | `text` (`json` in release) | encoding: text or json |
| `log.add-source` | `false` (`true` in dev) | annotate records with `file:line` |
| `log.async-buffer-size` | `4096` | records buffered before dropping starts |

Each optional sink is gated by its own `Enabled` flag rather than by a nil
pointer, because `config` cannot bind pointer fields.

**File sink** (`log.file.*`): `enabled`, `path`, `format`, `buffer-size`,
`flush-interval`, `max-size` (rotate above this many bytes), `max-backups`
(`0` keeps all).

**Graylog GELF sink** (`log.graylog.*`): `enabled`, `url`, `facility`,
`service` (blank uses the hostname), `timeout`, `index-fields` (blank sends
all).

**Sampling** (`log.sampling.*`): `enabled`, `tick` (the window), `initial`
(records that always pass per window), `thereafter` (after the burst, every
Nth record passes).

## API

| | |
| --- | --- |
| `Bootstrap() *slog.Logger` | a plain stderr logger for use before configuration is loaded |
| `New(Config) (*logger.Handler, *slog.LevelVar, error)` | build the configured handler |
| `(Config) Validate() error` | check level, format and sink settings |
| `Levels()` / `Formats()` | accepted names, for shell completion |
| `ParseLevel` / `ParseFormat` | name to `slog.Level` / `logger.Format` |
| `Error` | the `errs.Tag` this package wraps failures with |
