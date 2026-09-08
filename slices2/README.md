# slices2

Mapping over a slice, including the case where the mapping can fail.

The standard library has `slices` for searching, sorting and comparing, but no
`Map`. Writing the loop by hand is three lines, and two of them are the ones
people get wrong: the result slice is allocated without capacity, or the error
from element seventeen is dropped because the loop had nowhere to put it.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/slices2"
```

## Usage

```go
ids := slices2.Map(workers, func(w Worker) string { return w.ID })
```

```go
// Convert stops at the first failure and returns it.
addrs, err := slices2.Convert(raw, netip.ParseAddr)
if err != nil {
    return err
}
```

```go
// ConvertErrs keeps going and returns every failure, so a bad batch reports
// all of its bad rows rather than only the first.
rows, errs := slices2.ConvertErrs(lines, parseRow)
```

## Choosing between Convert and ConvertErrs

`Convert` is for a pipeline: the first bad element makes the whole result
meaningless, so there is nothing to gain from parsing the rest.

`ConvertErrs` is for a batch someone has to fix. Reporting one broken row at a
time turns a single correction pass into as many passes as there are broken
rows, which is the thing that makes an import job miserable.

## API

```go
func Map[In, Out any](xs []In, fn func(In) Out) []Out
func Convert[In, Out any](xs []In, fn func(In) (Out, error)) ([]Out, error)
func ConvertErrs[In, Out any](xs []In, fn func(In) (Out, error)) ([]Out, []error)
```
