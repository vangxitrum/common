# version

The build stamp linked into a binary, and the value the [debug
server](../README.md) serves on `/version/`.

A binary carries two independent things: the **version stamp** (`release`,
`sha`, `build`) and the **defaults channel** (`dev` or `release`), which picks
between the `devDefault` and `releaseDefault` struct tags read by
[`config`](../../config/README.md).

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/stats/version"
```

## Usage

Call `Register` once from `main`, before any command runs:

```go
func main() {
    version.Register()          // publishes the linked stamp to version.Build
    config.SetRelease(version.IsRelease())
    ...
}
```

`Register` is what makes a service's `version` subcommand and the debug
server's `/version/` agree.

## Stamping a build

A service's Makefile passes `-ldflags`:

```make
VERSION_PKG := gitlab.internal/tuan.quang.tran/aioz-common/stats/version

LDFLAGS := -s -w \
	-X $(VERSION_PKG).release=$(RELEASE) \
	-X $(VERSION_PKG).sha=$(SHA) \
	-X $(VERSION_PKG).build=$(BUILD_TIME) \
	-X $(VERSION_PKG).channel=$(CHANNEL)
```

**Those four must stay plain `string` variables.** The linker's `-X` can only
patch a symbol whose type is exactly `string`; aimed at a struct field such as
`version.Build.Release` it resolves to a symbol that does not exist and is
**silently dropped**, leaving a binary that reports `dev` with no error
anywhere. That is why the patched symbols are the unexported vars and `Register`
copies them into `Build` at runtime.

## Why the channel is a separate variable

Deriving the channel from the version stamp - "anything not `dev` is a release"
- looks reasonable and is wrong: a Makefile stamps `git describe --tags --always
--dirty` on every build, so every build looks like a release and no `devDefault`
value is ever reachable. That is not hypothetical; the template shipped that
bug, and the symptom was a developer running `make build` and getting JSON logs
at info level with no idea the readable colorized logger existed.

Because settings like `log.format` and `log.level` are `user:"true"`, the
channel that generated a config file is baked into it as live values. Generate
production config with a production binary - the container image - or set them
explicitly.

## API

| | |
| --- | --- |
| `Register()` | copy the linked stamp into `Build`; call once from `main` |
| `Build` | the `Info` the debug server serves on `/version/` |
| `Current() Info` | the linked stamp |
| `IsRelease() bool` | whether to use `releaseDefault` config tags |
| `Channel() string` | `"dev"` or `"release"` |
| `String() string` | the stamp rendered for a `version` subcommand |
| `(Info) Marshal() ([]byte, error)` | JSON encoding |
