# memory

A byte count that carries its unit.

It exists because `int64` does not say what it counts, and a configuration
field that means bytes is read by a person who assumes megabytes. `Size` parses
and prints with the prefix attached, so `2GiB` survives the round trip through
a config file, a flag and a log line without anyone having to remember the
scale.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/memory"
```

## Usage

```go
type Config struct {
    CacheSize memory.Size `help:"in-memory cache size" default:"2GiB"`
}

// Arithmetic is on the underlying int64, so the unit costs nothing.
buf := make([]byte, cfg.CacheSize.Int())

log.Info("cache sized", "size", cfg.CacheSize) // cache sized size=2.0 GiB
```

`Size` implements `flag.Value`, so it binds to a flag directly, and `Sizes`
collects a repeated flag into a list.

## Base 2 and base 10 are both real, and they are not the same

```go
memory.KiB // 1024
memory.KB  // 1000
```

Both sets of constants exist because both are used in the wild: a disk vendor's
terabyte is `TB`, a kernel's is `TiB`, and silently picking one is how a
capacity check ends up 10% wrong. `ParseString` accepts either, so a config
that says `1GB` and one that says `1GiB` are both valid and different, which is
the point.

`String` does not pick a family in advance. It prints in whichever of the two
gives the rounder number, so `1000000` reads as `1.0 MB` and `1048576` as
`1.0 MiB`, and a size that was written by a person comes back looking the way
they wrote it. Call `Base2String` or `Base10String` when the output has to be
in one family regardless.

## One sharp edge

`MarshalText` returns `(string, error)`, not `([]byte, error)`, so `Size` does
**not** satisfy `encoding.TextMarshaler`. `encoding/json` therefore ignores it
and uses `MarshalJSON`, which is present and correct; a library that reaches
for `TextMarshaler` alone will silently fall back to marshaling the underlying
`int64`. `UnmarshalText` has the standard signature and does satisfy
`encoding.TextUnmarshaler`, so the asymmetry only bites on the way out.

## API

`Size` is an `int64`, so it is comparable, usable as a map key and free to copy.

- **Parse and print**: `ParseString`, `FormatBytes`, `Size.String`,
  `Size.Base2String`, `Size.Base10String`.
- **Convert**: `Int`, `Int32`, `Int64`, `Float64`, and a named accessor per
  unit (`KiB`, `MiB`, `GiB`, `TiB`, `PiB`, `EiB`, `KB`, `MB`, `GB`, `TB`, `PB`,
  `EB`) returning `float64`.
- **Interfaces**: `flag.Value` (`Set`, `Type`), `MarshalText`/`UnmarshalText`,
  `MarshalJSON`/`UnmarshalJSON`.
- **`Sizes`**: a `flag.Value` collecting several sizes, with a `Default` used
  until the flag is set.
