# aioz-config

Configuration for AIOZ Go services: a config struct declares its own defaults,
help text and persistence rules through struct tags; the package binds it to
command-line flags, loads values from a YAML file and the environment, and
generates a default config file in which every setting is documented and every
untouched default is commented out.

Extracted from the DePIN hub's `internal/cfgstruct` + `pkg/process`, which in
turn derive from Storj. See [Differences from depin](#differences-from-depin).

## Requirements

- Go 1.25
- `cobra` for the command tree; `viper` and `pflag` come with it

## Installation

```sh
go get gitlab.internal/tuan.quang.tran/aioz-common
```

## Quick start

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	config "gitlab.internal/tuan.quang.tran/aioz-common/config"
)

type Config struct {
	Server struct {
		Address string `help:"address to listen on" default:"127.0.0.1:8080"`
		Timeout time.Duration `help:"request timeout" default:"30s"`
	}
	DataDir  string `path:"true" help:"where to keep state" default:"$CONFDIR/data"`
	Operator string `user:"true" help:"operator name"       default:""`
}

func main() {
	var cfg Config
	configDir := config.ApplicationDir("aioz", "myservice")

	root := &cobra.Command{Use: "myservice"}
	config.SetupFlag(
		nil, root, &configDir,
		"config-dir", configDir, "the directory to load the config from",
	)

	setup := &cobra.Command{
		Use:         "setup",
		Annotations: map[string]string{"type": "setup"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				return err
			}
			return config.SaveConfig(
				cmd,
				filepath.Join(configDir, config.DefaultCfgFilename),
			)
		},
	}
	run := &cobra.Command{
		Use: "run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := config.Load(cmd); err != nil {
				return err
			}
			fmt.Println("listening on", cfg.Server.Address)
			return nil
		},
	}
	root.AddCommand(setup, run)

	defaults := config.DefaultsFlag(root)
	config.Bind(setup, &cfg, defaults, config.SetupMode(), config.ConfDir(configDir))
	config.Bind(run, &cfg, defaults, config.ConfDir(configDir))

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
```

`myservice setup` writes:

```yaml
# where to keep state
# data-dir: /home/you/.local/share/aioz/myservice/data

# operator name
operator: ""

# address to listen on
# server.address: 127.0.0.1:8080

# request timeout
# server.timeout: 30s
```

`myservice run` then reads that file, the environment
(`AIOZ_SERVER_ADDRESS=...`) and the command line, in increasing order of
precedence.

## Where values come from

Highest precedence first:

1. an explicit command-line flag (`--server.address=...`)
2. an environment variable, `AIOZ_` + the key upper-cased with `.` and `-`
   turned into `_` (`AIOZ_SERVER_ADDRESS`). Override the prefix with
   `AIOZ_ENV_PREFIX` at runtime or `WithEnvPrefix` in code
3. `secrets.yaml` in the config directory, merged over
4. `config.yaml` in the config directory
5. the `default` struct tag

## Struct tags

| Tag | Effect |
| --- | --- |
| `help` | flag usage; also written as a comment above the setting in the generated file |
| `default` | the value, parsed according to the field's type |
| `devDefault` / `releaseDefault` / `testDefault` | build-specific values, see below |
| `flagname` | overrides the derived name; must match `^\p{Ll}[\p{Ll}\p{N}-]*$` |
| `noprefix:"true"` | do not prepend the parent struct's prefix |
| `noflag:"true"` | skip the field entirely |
| `internal:"true"` | no flag at all; setting a default alongside it panics |
| `setup:"true"` | bound only under `SetupMode()`, and never persisted |
| `hidden:"true"` | hidden from help, and never persisted |
| `deprecated:"true"` | hidden from help; persisted unless `SaveConfigRemovingDeprecated` |
| `user:"true"` | always written live in the generated file, even at its default |
| `source:"flag"` | command line only; never persisted |
| `path:"true"` | run the default through `filepath.FromSlash` |
| `basic-help:"true"` | include in short help |

Flag names come from the Go field name, snake-cased then hyphenated, and
prefixed by each enclosing named struct: `Server.MaxAttempts` becomes
`server.max-attempts`. An embedded struct is flattened instead, and **must be
an exported type** - reflection cannot address the fields of an unexported
embedded struct.

Supported field types: `int`, `int64`, `uint`, `uint64`, `float64`, `string`,
`bool`, `time.Duration`, `[]string`, arrays of structs, nested structs, and any
type implementing `pflag.Value`. Anything else panics at bind time, naming the
type, the flag and the Go field.

### Which default applies

`BindFlags` picks one of the four default tags:

| Build | Order tried |
| --- | --- |
| dev | `devDefault`, `default` |
| release | `releaseDefault`, `default` |
| test | `testDefault`, `devDefault`, `default` |

Defining only a default for a build you are *not* doing panics, rather than
silently binding the zero value. The build kind comes from `DefaultsFlag`,
which reads `--defaults` off the command line, falling back to `SetRelease`.
Release binaries should call `config.SetRelease(true)` from `main`.

### Directory variables

A default may reference `$CONFDIR`, `$IDENTITYDIR`, `$DATADIR` or any variable
set with `ConfigVar`, supplied as bind options:

```go
config.Bind(cmd, &cfg,
	config.ConfDir("/etc/myservice"),
	config.IdentityDir("/etc/myservice/identity"),
	config.ConfigVar("CACHE", "/var/cache/myservice"),
)
```

## The generated file

`SaveConfig` writes every persistable setting, sorted, with its help text as a
comment. A setting still at its default is written **commented out**, so that a
later change to that default still reaches nodes that never set it; a setting
that was explicitly chosen, loaded from an existing file, overridden, or tagged
`user:"true"`, is written live. `setup`, `hidden` and `source:"flag"` settings
are never written at all.

A setup command that may run against an existing config should `Load` before
`SaveConfig`, so that regenerating preserves what the operator already set.

## API

| Function | Purpose |
| --- | --- |
| `Bind(cmd, &cfg, opts...)` | bind a struct to a cobra command and record it for `Load` |
| `BindFlags(flags, &cfg, opts...)` | bind to a bare `pflag.FlagSet`, for tests and harnesses |
| `Load(cmd, opts...) (*LoadResult, error)` | read files and environment into every struct bound to `cmd` |
| `SaveConfig(cmd, path, opts...)` | write the config file |
| `Viper(cmd, opts...)` | the underlying viper, built on first use |
| `ReadConfigFiles(cmd, vip)` | the default file-reading step, for use inside a custom loader |
| `New() *Registry` | an isolated registry, for tests or long-lived processes |
| `Forget(cmd)` | drop a command's config structs and memoized viper |
| `ApplicationDir`, `IsValidSetupDir`, `IsWritable`, `AtomicWriteFile` | filesystem helpers for setup commands |

`Load` must run after cobra has parsed flags - from `RunE`, `PreRunE` or
`PersistentPreRunE` - because it reads `--config-dir` to find the file. Calling
it earlier returns `ErrFlagsNotParsed` rather than silently reading nothing.

`LoadResult` reports what happened to every key:

```go
res, err := config.Load(cmd, config.WithFailOnValueError())
res.UsedKeys    // decoded into a struct or set on a flag
res.MissingKeys // in the file but matching nothing; usually a typo
res.BrokenKeys  // matched something, but the value would not parse
```

Options: `WithConfigLoader`, `WithLogger`, `WithEnvPrefix`, `WithFilenames`,
`WithFailOnValueError`.

### Registries

The package-level functions share one default registry, which is all a normal
binary needs. Because it is keyed by `*cobra.Command` and never expires, a
long-lived process that builds commands dynamically should either use its own
`New()` registry or call `Forget` when a command is done. Tests that run in
parallel should use their own registry.

## Differences from depin

The port is faithful in behavior, but not identical:

- `cfgstruct.Bind` is now `BindFlags`; `process.Bind` keeps the name `Bind`.
- `process.LoadConfig` is now `ReadConfigFiles`, since the new `Load` does
  something different.
- The `Prefix` bind option is now `FlagPrefix`.
- The decode step that lived inside `process.InitBeforeExecute` is now the
  exported `Load`, and reports its key sets instead of only logging them.
- Package-level `configs`/`vipers` maps became `Registry`, which fixes a
  deadlock when a custom loader called back into the package, and a leak that
  pinned every command forever.
- Process lifecycle (`Exec`, `Ctx`, logging, tracing, debug server, monkit)
  stays behind; this package is configuration only. A service adopting it keeps
  its own thin lifecycle layer that calls into `Bind`/`Load`/`SaveConfig`.
- `go.uber.org/zap` is gone; `SetupFlag` takes a `*slog.Logger` (nil is fine).

Two bugs are fixed rather than reproduced, so behavior differs from depin:

1. A value that failed to parse used to be marked as set anyway, so the next
   generated config persisted the bad value live, producing a file the binary
   itself would then reject.
2. A list-valued setting claimed by no struct used to be read as a string,
   which viper cannot cast, so the list was silently emptied.

`DefaultsType` also used to contain `if false { return "release" }`, so every
binary ran on dev defaults; it now follows `SetRelease`.

## Development

```sh
make lint      # golangci-lint, the same version CI pins
make lint-fix
make test
```
