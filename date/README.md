# date

Month and day boundaries, computed in UTC.

It exists because billing and reporting ask the same question in a dozen
places - where does this month start, where does it end, how many months lie
between these two dates - and every hand-rolled answer gets the edges slightly
differently. An end-of-month computed as "the 30th" is wrong four times a year;
one computed by adding a month and subtracting a nanosecond is right every
time.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/date"
```

## Usage

```go
start, end := date.MonthBoundary(time.Now())
rows, err := db.UsageBetween(ctx, start, end)
```

```go
// A period identifier from a report or an invoice, back into a time.
t, err := date.PeriodToTime("2026-03")
```

## Everything here is UTC

`UTCBeginOfMonth` and `UTCEndOfMonth` say so in the name; `MonthBoundary` and
`DayBoundary` do the same without saying it. That is deliberate and it is the
trap worth naming: a boundary computed in a server's local zone moves when the
server moves, so two nodes in different regions disagree about which month a
usage record belongs to. Accounting periods are UTC or they are not
reproducible.

## API

- `MonthBoundary(t)` and `DayBoundary(t)` return the inclusive start and end of
  the month or day containing `t`.
- `UTCBeginOfMonth(t)`, `UTCEndOfMonth(t)` are the same edges, named.
- `MonthsBetweenDates(from, to)` and `MonthsCountSince(from)` count whole
  months.
- `PeriodToTime("YYYY-MM")` parses a period identifier. See
  [`period`](../period/README.md) for the value type that owns that format.
- `TruncateToHourInNano(t)` truncates to the hour and returns Unix nanoseconds,
  for bucketing time series.
