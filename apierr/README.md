# apierr

Errors that cross the HTTP boundary. An `apierr` carries three things a Go
error alone does not: the HTTP status to answer with, a machine-readable reason
slug the client can branch on without parsing prose, and the underlying error
for the log. The transport renders the first two into the JSON envelope;
nothing else has to know how a given failure maps to a status code.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/apierr"
```

A slice classifies its own failures; the transport renders them. An endpoint
never picks a status code - it returns the service's error unchanged.
## The one rule

**A reason names the subject, not the status.** `contract-not-found`, never
`not-found`. One status maps to many reasons, never the reverse.

```go
// no  - a client cannot tell this from any other 404
return apierr.NotFound("not-found", "no such contract")

// yes - and the operator gets the id
return apierr.NotFound("contract-not-found", "no such contract").
    With("resource", "contract", "id", contractID)
```

A test parses every `.go` file in the module and rejects the first form, so
this is enforced rather than remembered.

## Which constructor

| the situation | use | status |
| --- | --- | --- |
| the client sent something unusable | `BadRequest` | 400 |
| the client did not prove who it is | `Unauthorized` | 401 |
| the client is known but not allowed | `Forbidden` | 403 |
| the thing does not exist | `NotFound` | 404 |
| the request fights the current state | `Conflict` | 409 |
| the client is going too fast | `TooManyRequests` | 429 |
| something on our side broke | `Internal` | 500 |
| a dependency is down, or we are still starting | `Unavailable` | 503 |

`New(reason, status, format, ...)` covers a status not in the list.

## Recipes

**Validation** - a message the client should read:

```go
if len(name) > s.config.MaxNameLength {
    return apierr.BadRequest("name-too-long",
        "name must be at most %d bytes", s.config.MaxNameLength).
        With("limit", s.config.MaxNameLength, "got", len(name))
}
```

**A missing record** - classify the repository's sentinel, keep it unwrappable:

```go
contract, err := s.contracts.GetByID(ctx, id)
if err != nil {
    if errors.Is(err, store.ErrNotFound) {
        return apierr.NotFound("contract-not-found", "no such contract").
            With("resource", "contract", "id", id)
    }
    // Not a missing row: a real fault. Wrap it, do not describe it.
    return apierr.Internal("contract-load-failed", err, "id", id)
}
```

Note the two branches produce different reasons. Collapsing them is exactly the
bug described below.

**A dependency failing** - `Wrap` keeps `errors.Is` and `errors.As` working
through the apierr, so a caller can still match the underlying cause:

```go
if err := s.payments.Charge(ctx, amount); err != nil {
    return apierr.Wrapf("charge-failed", http.StatusBadGateway, err,
        "charging %s", accountID)
}
```

**Wrap is nil-safe**, so this is fine on a bare return path:

```go
return apierr.Wrap("contract-save-failed", http.StatusInternalServerError, err)
```

## Do not

```go
// the endpoint inventing a status the service already decided
if err != nil {
    return echo.NewHTTPError(http.StatusBadRequest, err.Error())
}

// a reason that restates the status - the policy test rejects this
return apierr.Forbidden("forbidden", "denied")

// describing a cause instead of wrapping it: errors.Is stops working,
// and the stack is lost
return apierr.Internal("db-error", errors.New(err.Error()))

// putting the subject in the message where only a human can find it
return apierr.NotFound("not-found", "contract %s not found", id)
```

## Reading an error

```go
apierr.Status(err)   // the HTTP status; 500 for anything unclassified
apierr.Reason(err)   // the slug, or "" 
apierr.Fields(err)   // the With pairs, for logs
apierr.Tags(err)     // packages the error passed through, from errs.Tag
apierr.Trace(err)    // the origin stack, or ""
errors.Is/As         // still work through every apierr
```

`Tags` and `Trace` walk the `Unwrap` chain on purpose: `errs.Tags` type-asserts
on the error it is handed and gives up if it is not an errs error, and a stack
only prints under `%+v`. Wrapping an errs error in anything - an apierr, a
plain `fmt.Errorf` - makes the naive versions of both report nothing, with no
sign at the call site.

## What the client sees, and what you get

A 4xx message was written for the client, so it is sent as-is:

```json
{"error":{"code":400,"reason":"name-too-long","message":"name must be at most 64 bytes","request_id":"..."}}
```

## Messages are for people

The frontend puts `message` in front of a user unchanged, so the transport
formats every one: a capital first letter and a full stop, applied at the point
the response is written. An author never has to remember it, and messages echo
generates are treated the same - `Not Found` becomes `Not Found.`

One exception, on purpose. A message that begins with a field name is left
uncapitalized, because `Sort_by must be...` reads as a bug rather than as a
sentence. Write the message as prose instead and let the identifier sit in the
middle:

```go
// reads badly on screen, and the formatter will not rescue it
apierr.BadRequest("sort-by-unknown", "sort_by must be one of name, greeting")

// reads as a sentence
apierr.BadRequest("sort-by-unknown", "the sort_by parameter must be one of: name, greeting")
```

**The client never sees the wrapped cause - on any status.** `Error()` is the
operator's view and appends it; the transport renders `ClientMessage()`, which
is only what the author wrote. This matters most where it is easiest to miss:

```go
// the cause is a parser complaining about raw input
return apierr.Wrapf("list-query-invalid", http.StatusBadRequest, err,
    "the query parameters are invalid")
```

The user reads *The query parameters are invalid.*; the log keeps
`the query parameters are invalid: code=400, message=strconv.ParseInt: parsing
"notanumber": invalid syntax`.

**A 5xx tells the client nothing but the reason.** Its message was written for
an operator and routinely carries a driver error or a hostname, so the
transport replaces it with `internal server error`. The client keeps the reason
slug and the request id - what a support conversation needs. Everything else
goes to the log:

```
level=ERROR msg="request failed" reason=contract-load-failed
  tags=["repo"] stack="repo: connection refused\n\t...Routes.func1:29"
  resource=contract id=c-42 request_id=bBySk...
```

`tags` is the packages it passed through; `stack` is where it started.
`zeebo/errs` captures a stack on the first `Tag.Wrap` and keeps it through
later ones, so wrap freely - the origin survives. `With` fields are appended,
which is how `resource` and `id` got there.

Failures are also counted on `/metrics` as `request_failures`, tagged by route
and reason, so a spike in one cause is visible without reading logs. The slugs
are chosen in code, so the tag set is closed and cannot blow up cardinality.

**An unclassified error is a 500.** A failure nobody labelled is a surprise,
and a surprise is the server's fault until someone says otherwise. The
corollary: a permission check must return `apierr.Forbidden`, or it answers 500
and a permission problem reads as an outage.

## Why the rule exists

A tempting model gives an error two axes - the package it came from (`file`)
and the status it maps to (`not found`) - and nothing that carries the
*subject*. Then a *contract* lookup inside the file service returns
`FileError.Wrap(NotFound)`, which renders as **"file: not found"** for a
missing contract: the package name stands in for the subject and is wrong
whenever a slice looks up something that is not its own entity. If the status
tag is itself an error value, wrapping it also discards the real error - the
one that knew the contract id. And at the API boundary every missing record
collapses to the single slug `"not-found"`, so a client cannot tell a missing
contract from a missing file at all.

`With` is the third axis that model lacks, and the policy test is what stops
the slug collapsing back to the status. This is not hypothetical - it is a
failure mode taken from a production service.
