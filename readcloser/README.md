# readcloser

`io.Reader` combinators that also close.

The standard library has `io.MultiReader`, `io.LimitReader` and a reader that
fails, and none of them are `io.ReadCloser`. Wrapping one to add a `Close` is
where the underlying readers stop being closed, which is the leak. These four
carry the `Close` through.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/readcloser"
```

## Usage

```go
// Concatenate, and close each part as it is exhausted.
r := readcloser.MultiReadCloser(first, second, third)
defer r.Close()
```

```go
// Do not open the connection until somebody actually reads.
r := readcloser.LazyReadCloser(func() (io.ReadCloser, error) {
    return dial(ctx, addr)
})
```

```go
// Turn a failure into something that satisfies the interface, so the caller's
// normal error path reports it instead of a special case at construction.
if err != nil {
    return readcloser.FatalReadCloser(err)
}
```

## What each one is for

**`MultiReadCloser`** concatenates and closes each reader as it reaches EOF,
with the remainder closed by `Close`. It flattens nested instances, so building
one incrementally does not build a deep chain.

**`LazyReadCloser`** defers the expensive part until the first `Read`. A
consumer that ends up not reading pays nothing, which matters when the backing
reader is a dial or a file open.

**`LimitReadCloser`** is `io.LimitReader` that still closes the source.

**`FatalReadCloser`** always fails with the given error. It exists so a
constructor that cannot produce a reader still returns one, and the error
surfaces at the read, on the path that already handles read errors.

## API

```go
func MultiReadCloser(readers ...io.ReadCloser) io.ReadCloser
func LazyReadCloser(reader func() (io.ReadCloser, error)) io.ReadCloser
func LimitReadCloser(r io.ReadCloser, n int64) io.ReadCloser
func FatalReadCloser(err error) io.ReadCloser
```
