# Changelog

## v0.3.0 (unreleased)

Twelve utility packages imported from `depin`, three new packages, and one
security rule added to `config`. Everything is additive except the move of
`stats/version`, which leaves aliases behind.

### Added

**Twelve packages imported from depin**, with their commit history, keeping
their original package names so a later migration off depin's copies is an
import path rewrite rather than an API rewrite:

`base58`, `context2`, `date`, `errs2`, `leak`, `lrucache`, `memory`, `period`,
`readcloser`, `signal`, `slices2`, `time2`.

**`secret`** - bearer secrets that are never at rest in a replayable form.
`Token` is plaintext and redacts itself through fmt, slog, JSON and SQL;
`Hash` is what you store. Prefixed API keys carry a public lookup id, so
verification is one indexed row rather than a scan over every stored hash.
Recovery codes use Crockford base32, which folds the characters people
mistype. See [`secret/README.md`](secret/README.md).

**`jwt`** - signed tokens for a verifier that cannot reach the issuer's
database, with a shared secret or a key pair. The algorithm is pinned at
construction and the header's `alg` is never trusted, closing algorithm
confusion and `alg: none`. An expiry is required on both sides. `Ring`
rotates keys on `kid` without invalidating tokens in flight. See
[`jwt/README.md`](jwt/README.md).

**`version.SemVer`** - a comparable semantic version, tolerant on input
because internal builds disagree about the leading `v`. An untagged build now
derives `v0.0.0-dev.<unix>.g<commit>`, which sorts below every real tag.

**`version.Register`** - the build stamp as a monkit `StatSource`, so
`/metrics` carries a `version_info` series.

**`config` secret handling** - `secret:"true"` / `secret:"optional"` on a
field, and `config.CheckSecrets` at startup. See below.

**`stats/testcontext`** gained `Go`, `Wait` and `Cleanup` for tracking
background goroutines in a test.

### Changed

**`stats/version` moved to `version`.** It stopped being a stats concern once
it grew version comparison and a metrics source. `stats/version` remains as
type aliases with deprecation notices, so no import breaks.

**`Info.Version` is now a `SemVer`, not a `string`.** Rendering is unchanged -
it still marshals as a `v`-prefixed string - but it can now be compared. Code
that treated the field as a string needs `.String()`.

**A `secret`-tagged config field may no longer carry a `default` or
`releaseDefault`.** `Bind` panics if it does. This is a deliberate breaking
change for any struct that shipped a placeholder secret, which is the point:
a default reaches production, so a placeholder password in the source tree is
the password of every deployment that never overrode it.

To keep a local convenience value, pair `devDefault` with an explicitly empty
`releaseDefault`:

```go
Password string `secret:"true" devDefault:"dev-only" releaseDefault:""`
```

### Fixed

`stats/README.md` documented `-ldflags -X` targeting struct fields
(`...version.Build.Release=`). The linker only patches plain `string`
variables, so those flags were silently doing nothing. The corrected
invocation targets package-level variables in the new `version` package, and
uses `git describe --abbrev=0`, since bare `git describe` output sorts below
its own tag under semver rules.

### Dependencies

Three modules added, each with no dependencies of its own:
`blang/semver/v4`, `golang-jwt/jwt/v5`, and `zeebo/errs/v2` promoted to
direct use. `zeebo/errs` v1, which arrived with the imported packages, was
converted away rather than added.

### Migration

See [`docs/ADOPTING.md`](docs/ADOPTING.md).

| Old | New |
| --- | --- |
| `aioz-common/stats/version` | `aioz-common/version` |
| `info.Version` (string) | `info.Version.String()` |
| `-X .../stats/version.Build.X=` | `-X .../version.buildVersion=` |

## v0.2.0

`uuid` and `currency`; the build stamp derived from Go build info; `apierr`
trimmed to its core.

## v0.1.0

The initial module, assembled from `aioz-config`, `aioz-logger`, `aioz-stats`
and `aioz-template`'s `internal/utils`.
