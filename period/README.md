# period

A calendar month as a value, not a string.

A billing period is passed around as `"2026-03"` until someone writes
`"2026-3"`, or compares two of them with `<`, or parses one in a local time
zone. `Period` makes the format a type: it is validated once at the edge, it is
comparable, and it knows the half-open time range it stands for.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/period"
```

## Usage

```go
p, err := period.FromString("2026-03")
if err != nil {
    return err
}

rows, err := db.UsageIn(ctx, p.StartDate(), p.EndDateExclusive())
```

```go
p := period.FromTime(time.Now())   // the period containing that instant
fmt.Println(p)                     // 2026-03
```

## The range is half-open, and that is the whole point

```go
p.StartDate()         // <= every instant in the period
p.EndDateExclusive()  // >  every instant in the period
```

An inclusive end date has to be "the last nanosecond of the month", which
nobody writes correctly and which changes with the clock's resolution.
`StartDate <= t < EndDateExclusive` needs no such value, tiles with the next
period leaving no gap and no overlap, and translates directly into the SQL a
usage query wants.

`IsZero` reports on the year, not the month, because a `Period` built from a
bad month string still has a real year and would otherwise look like the zero
value.

## API

- `FromString(s)` parses `YYYY-MM`; `FromTime(t)` takes the period containing
  `t`.
- `StartDate()`, `EndDateExclusive()` give the half-open range.
- `String()` renders `YYYY-MM`; `MarshalCSV`/`UnmarshalCSV` carry it through a
  report.
- `IsZero()` reports whether this is the zero value.

## Storage

`Value` writes the `YYYY-MM` string and `Scan` reads it back, so a `Period` can
be a column. Without them it could not be stored at all: `database/sql` has no
converter for a struct, and the failure arrives at the first query rather than
at compile time.

The zero `Period` stores as `NULL`, not as `"0000-00"`. "No period" is a real
state - there is nothing outstanding to close - and `0000-00` is not a month.
