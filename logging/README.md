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
log := logging.Bootstrap()   // stderr, text, info, redacting - for use before config loads

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
| `log.redact` | (none) | attribute keys logged as `[redacted]`, beyond the built-in list |

Each optional sink is gated by its own `Enabled` flag rather than by a nil
pointer, because `config` cannot bind pointer fields.

**File sink** (`log.file.*`): `enabled`, `path`, `format`, `buffer-size`,
`flush-interval`, `max-size` (rotate above this many bytes), `max-backups`
(`0` keeps all).

**Graylog GELF sink** (`log.graylog.*`): `enabled`, `url`, `facility`,
`service` (blank uses the hostname), `timeout`, `index-fields` (blank sends
all).

**Elasticsearch sink** (`log.elasticsearch.*`): `enabled`, `url` (base URL, no
path), `data-stream`, `bearer-token` / `api-key` / `username` + `password`,
`batch-size`, `flush-interval`, `timeout`, `index-fields`, `service`, `role`,
`env`. Blank `service` uses the hostname; `host.name` is always the hostname.
`env` is stamped as `service.environment` and must be `staging`, `production`
or blank (no field): staging and production of one service share a data
stream, and this is the only field that tells their records apart.

**Sampling** (`log.sampling.*`): `enabled`, `tick` (the window), `initial`
(records that always pass per window), `thereafter` (after the burst, every
Nth record passes).

## Redaction

`New` and `Bootstrap` log the value of a sensitive attribute as `[redacted]`,
in every output - stdout, file and Graylog - and whether the attribute came
from `With`, from the context or from the record:

```go
log.Info("login", "email", email, "password", pw)
// ... email=alice@example.com password=[redacted]
```

A key is sensitive when, lowercased and with `-` written as `_`, it is one of
`RedactKeys()` or `log.redact` **exactly**: `password`, `passwd`, `secret`,
`client_secret`, `api_secret`, `secret_key`, `private_key`, `token`,
`access_token`, `refresh_token`, `id_token`, `auth_token`, `authorization`,
`proxy_authorization`, `cookie`, `set_cookie`, `otp`, `api_key`, `x_api_key`.
Every attribute inside a group with one of those names is redacted too.

Exact, not substring: `passwordless` and `token_count` are not secrets, and a
logger that redacts them teaches people to ignore `[redacted]`. `code`, `key`
and `session` are too common to claim; add them to `log.redact` where they are
secrets. Nothing is allocated for an attribute that is left alone.

This is a **safety net**, not the control. It sees keys, not values: a token
inside an `http.Header`, a struct or a request body is logged as it is. Keep
secrets in [`secret.Token`](../secret/README.md), which redacts itself
wherever it is formatted, and never log whole headers or bodies. A public
value under a sensitive name (a public key logged as `api_key`) is redacted:
rename the attribute.

## API

| | |
| --- | --- |
| `Bootstrap() *slog.Logger` | a plain, redacting stderr logger for use before configuration is loaded |
| `New(Config) (*logger.Handler, *slog.LevelVar, error)` | build the configured, redacting handler |
| `(Config) Validate() error` | check level, format and sink settings |
| `Levels()` / `Formats()` | accepted names, for shell completion |
| `ParseLevel` / `ParseFormat` | name to `slog.Level` / `logger.Format` |
| `RedactAttr(extra ...string)` | the `ReplaceAttr` that `New` and `Bootstrap` install, for a handler built by hand |
| `RedactKeys() []string` | the built-in sensitive keys |
| `Redacted` | `"[redacted]"` |
| `Error` | the `errs.Tag` this package wraps failures with |
