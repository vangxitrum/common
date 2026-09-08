# leak

Finds the resource nobody closed, and points at the line that opened it.

A leaked connection or file handle is discovered long after the fact, in a
metric or an `ulimit` error, with no way back to the code that created it.
`leak` records the stack at creation and complains at close time if a child was
never closed, so the report names the opener rather than the process.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/leak"
```

## Usage

```go
func NewPool(ctx context.Context) (*Pool, context.Context) {
    ref, ctx := leak.WithContext(ctx)
    return &Pool{ref: ref}, ctx
}

func (p *Pool) Dial(addr string) (*Conn, error) {
    child := p.ref.Child("conn:"+addr, 1)
    // ... and Conn.Close closes child
}

func (p *Pool) Close() error {
    return p.ref.Close() // errors if any child is still open
}
```

`Ref.Close` reports the children that are still open, each with the
`StartStack` captured when it was created.

## It only does anything under -race

`Ref` is an empty struct in a normal build, so every method is a no-op the
compiler removes: tracking is on only when compiling with `-race`. That is the
deal it offers - the bookkeeping is free in production and available in tests
and CI, where a leak is cheap to fix.

Note that this repository currently ships only the no-op implementation, and
its build constraint is commented out, so the tracking half is not built even
under `-race`. The API is stable and the call sites are correct; restoring the
race build is a change to this package alone.

## API

```go
func Root(skipCallers int) Ref
func WithContext(parent context.Context) (Ref, context.Context)
func FromContext(parent context.Context) Ref

func (ref Ref) Child(name string, skipCallers int) Ref
func (ref Ref) Close() error
func (ref Ref) StartStack() string
```

`skipCallers` is how many frames to drop from the recorded stack, so the
reported line is your caller's rather than your constructor's.
