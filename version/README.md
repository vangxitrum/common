# version

The build stamp of the running binary, and the value the [debug
server](../README.md) serves on `/version/`.

**A service does not have to wire anything into its Makefile.** Since Go 1.18
`go build` embeds the commit, the commit timestamp and whether the working tree
was dirty into every binary, and `debug.ReadBuildInfo` reads them back. The
`-ldflags -X` variables remain, but only as a fallback for builds where that
information is absent.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/version"
```

## Usage

`Build` is populated in `init`, so there is nothing to call:

```go
fmt.Print(version.Build)                 // for a version subcommand
config.SetRelease(version.IsRelease())   // pick releaseDefault config tags
```

## Where each field comes from

| field | source |
| --- | --- |
| `CommitHash` | `vcs.revision` from the build info |
| `Timestamp` | `vcs.time` |
| `Modified` | `vcs.modified` - the working tree was dirty |
| `Version` | the module version, but only for a binary installed with `go install module@v1.2.3`. Go synthesises a pseudo-version for a local build, which records that no version was set, so it is discarded. |
| `Release` | derived, see below |

## Release is derived, not declared

A build counts as a release only if it is provably a clean, tagged one: a real
version, a recorded commit and timestamp, and an unmodified tree. A binary built
from a dirty checkout says so rather than claiming to be a release.

This is the point of reading the toolchain's own record instead of trusting a
flag - a binary cannot assert a provenance it does not have.

## The fallback

The Go toolchain omits the VCS block when the build is not made from a
repository: `-buildvcs=false`, a container build whose context has no `.git`,
and **always inside `go test`**. The link-time variables cover those cases:

```make
VERSION_PKG := gitlab.internal/tuan.quang.tran/aioz-common/version

LDFLAGS := -s -w \
	-X $(VERSION_PKG).buildVersion=$(VERSION) \
	-X $(VERSION_PKG).buildCommitHash=$(SHA) \
	-X $(VERSION_PKG).buildTimestamp=$(shell date -u +%s) \
	-X $(VERSION_PKG).channel=$(CHANNEL)
```

Each of `buildVersion`, `buildCommitHash` and `buildTimestamp` is consulted only
when the build info did not already supply that field. `buildRelease` is the one
exception: set it and it overrides the derivation outright.

**Those variables must stay plain `string`s.** The linker's `-X` can only patch
a symbol whose type is exactly `string`; aimed at a struct field such as
`Build.Version` it resolves to a symbol that does not exist and is **silently
dropped**.

## The defaults channel

`channel` is separate from `Release` and is the one thing still worth setting
explicitly. It selects which [`config`](../../config/README.md) struct tags
apply - `devDefault` or `releaseDefault` - and that is a deliberate choice about
how the binary should behave, not an observation about how it was built. `make
build` leaves it at `dev`; a Dockerfile or a release job passes
`CHANNEL=release`.

Deriving it from the version stamp - "anything not `dev` is a release" - looks
reasonable and is wrong: a Makefile that stamps `git describe --tags --always
--dirty` makes every build look like a release, and no `devDefault` value is
ever reachable. The template shipped that bug; the symptom was a developer
running `make build` and getting JSON logs at info level with no idea the
readable colorized logger existed.

Because settings like `log.format` and `log.level` are `user:"true"`, the
channel that generated a config file is baked into it as live values. Generate
production config with a production binary, or set them explicitly.

## API

| | |
| --- | --- |
| `Build` | the `Info` for this binary, filled in at startup |
| `Info` | `Version`, `CommitHash`, `Timestamp`, `Release`, `Modified` |
| `(Info) String() string` | the stamp rendered for a `version` subcommand |
| `(Info) Marshal() ([]byte, error)` | JSON encoding, as served on `/version/` |
| `IsRelease() bool` | whether to use `releaseDefault` config tags |
| `Channel() string` | `"dev"` or `"release"` |

## Comparing versions

`Info.Version` is a `SemVer`, not a string, so it can be ordered:

```go
if version.Build.Version.Compare(minimum) < 0 {
    return fmt.Errorf("build %s is below the minimum %s", version.Build.Version, minimum)
}
```

`NewSemVer` is tolerant, because an internal service's version arrives from a
Makefile, a CI variable, a Docker label or a `go install` path and those
disagree about the leading `v` and about how many components they write.
`v1.2.3`, `1.2.3`, `v1.2` and `v1` all parse. What they are not allowed to
disagree on is the ordering, which is the reason the field is a value and not
the string that came in.

The zero value renders and marshals as `""`, not `v0.0.0`, so "no version"
stays distinguishable from "version zero".

### An untagged build still gets a version

A build with no tag would otherwise have nothing to compare, so the stamp
derives one:

```
v0.0.0-dev.1754300000.g702ee23
```

Every part is load-bearing. The base is `v0.0.0` and `dev` is a **prerelease**,
so semver's rule that a prerelease sorts below its release puts every derived
version below every real tag, `v0.0.0` included. An updater comparing versions
therefore always sees a release as newer. The timestamp is a numeric
identifier, which semver compares numerically rather than as text, so two
derived versions order by build time. The commit is prefixed with `g` because a
semver identifier may not be an all-numeric string with a leading zero, and a
short hash can easily be all digits.

`SemVer.IsDev` reports whether a version came from here rather than from a tag.

### Do not feed `git describe` output in as the version

```
v2.4.1-3-gabc1234   // three commits AFTER v2.4.1
```

It parses. It also sorts **below** `v2.4.1`, because semver reads everything
after the dash as a prerelease. A service stamped this way believes it is older
than the tag it was built past. There is a test asserting this, so the trap
stays visible. Use a real tag, or let the derived dev version do its job.

## Metrics

```go
version.Register(nil)   // nil means monkit.Default
```

That is the whole wiring, and `/metrics` then carries a `version_info` series:

```
version_info{scope="",field="release"} 0
version_info{scope="",field="dev"} 0
version_info{scope="",field="major"} 2
version_info{scope="",field="minor"} 4
version_info{scope="",field="patch"} 1
version_info{scope="",field="os_linux"} 1
version_info{scope="",field="arch_amd64"} 1
```

Prometheus stores `float64` and nothing else, so every field is encoded to
survive that:

| Field | What it is |
| --- | --- |
| `major`, `minor`, `patch` | the version as three numbers, so it can be compared and graphed |
| `commit` | `crc32` of the commit hash. Meaningless alone, but it changes exactly when the commit does, so "how many distinct builds are live" is a count of distinct values |
| `timestamp`, `age_sec` | when the commit was made, and how long ago. `age_sec` is what shows an instance was left behind, without the query needing to know the current version |
| `release`, `modified`, `dev` | 1 or 0 |
| `os_<goos>`, `arch_<goarch>` | a field *named* for its value, set to 1, so a query can group by platform |

Fields the build does not know are omitted rather than sent as zero, so a
missing commit does not look like commit zero.

## Wiring the version in

A plain `go build` gives you the commit, the build time and whether the tree was
dirty, with no Makefile at all. It does **not** give you the tag: `bi.Main.Version`
is only set by `go install module@version`, and Go synthesizes a meaningless
pseudo-version otherwise, which this package discards.

So a semantic version needs one link-time flag. The target is an unexported
package variable, which nobody guesses:

```makefile
VERSION_PKG := gitlab.internal/tuan.quang.tran/aioz-common/version
LDFLAGS := -X $(VERSION_PKG).buildVersion=$(shell git describe --tags --abbrev=0) \
           -X $(VERSION_PKG).channel=release

build:
	go build -ldflags "$(LDFLAGS)" ./cmd/...
```

Note `--abbrev=0`, which yields the bare tag rather than the `v2.4.1-3-gabc1234`
form described above.

`-X` must target a plain `string` variable. Aimed at a struct field such as
`Build.Version` it names a symbol that does not exist and the linker **silently
drops it**, leaving no error and no value.

## Storage

`SemVer` implements `driver.Valuer` and `sql.Scanner`, writing the rendered form
with its leading `v`. Without them it could not be a column: `database/sql` has
no converter for a struct, and the embedded blang `Version` supplies none.

Scanning is as tolerant as `NewSemVer`, so a row written by an older release
that omitted the `v` or the patch component still reads.

The zero value stores as `NULL` rather than as `v0.0.0`, for the same reason
`String` renders it empty: "no version" and "version zero" are different facts,
and a column that cannot tell them apart has lost one.
