# apierr

An error that crosses the HTTP boundary. It carries three things a Go error
alone does not: the HTTP status to answer with, a machine-readable reason slug
the client can branch on without parsing prose, and the underlying error for the
log.

This package provides the type and the helpers to read it. It does not police
what you put in it - a service defines its own reasons and, if it wants, its own
constructors.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/apierr"
```

## Defining your own errors

The eight named constructors cover the usual statuses, and `New` covers the
rest. A service is expected to build its own vocabulary on top:

```go
package contract

import (
	"net/http"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
)

// NotFound is this slice's own constructor. The reason names the subject, so a
// client can tell a missing contract from any other 404.
func NotFound(id string) *apierr.Error {
	return apierr.NotFound("contract-not-found", "no such contract").
		With("resource", "contract", "id", id)
}

func Expired(id string) *apierr.Error {
	return apierr.New("contract-expired", http.StatusGone, "the contract has expired").
		With("id", id)
}
```

**A reason should name the subject, not the status.** `contract-not-found`, not
`not-found`: one status maps to many reasons, never the reverse. Collapsing
every missing thing onto the single slug `not-found` leaves a client unable to
tell them apart. That is a convention here, not an enforced rule - a service
that wants it checked should write its own test over its own packages.

## Constructors

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

## Wrapping a cause

`Wrap` keeps `errors.Is` and `errors.As` working through the apierr, so a caller
can still match the underlying cause. It is nil-safe, so it is fine on a bare
return path:

```go
if err := s.payments.Charge(ctx, amount); err != nil {
    return apierr.Wrapf("charge-failed", http.StatusBadGateway, err,
        "charging %s", accountID)
}

return apierr.Wrap("contract-save-failed", http.StatusInternalServerError, err)
```

Wrap the cause; do not describe it. `apierr.Internal("db-error", errors.New(err.Error()))`
throws the stack away.

## Reading an error

```go
apierr.Status(err)   // the HTTP status; 500 for anything unclassified
apierr.Reason(err)   // the slug, or ""
apierr.Fields(err)   // the With pairs, for logs
apierr.Tags(err)     // packages the error passed through, from errs.Tag
apierr.Trace(err)    // the origin stack, or ""
errors.Is / errors.As  // still work through every apierr
```

`Tags` and `Trace` walk the `Unwrap` chain on purpose. `errs.Tags` type-asserts
on the error it is handed and gives up if it is not an errs error, and a stack
only prints under `%+v`. Wrapping an errs error in anything - an apierr, a plain
`fmt.Errorf` - makes the naive versions of both report nothing, with no sign at
the call site. `*Error` also implements `Format`, so `%+v` on an apierr reaches
the cause's stack.

## What the client sees

`Error()` is the operator's view and appends the wrapped cause.
`ClientMessage()` is only what the author wrote, and is what a transport should
render:

```json
{"error":{"code":400,"reason":"name-too-long","message":"name must be at most 64 bytes"}}
```

**The client never sees the wrapped cause, on any status.** For a 5xx the
message was written for an operator and routinely carries a driver error or a
hostname, so a transport should replace it with `internal server error` and
leave the client the reason slug. **An unclassified error is a 500** - a failure
nobody labelled is a surprise, and a surprise is the server's fault until
someone says otherwise. The corollary: a permission check must return
`apierr.Forbidden`, or it answers 500 and a permission problem reads as an
outage.

Rendering the envelope - capitalising messages, attaching a request id, counting
failures by reason - is the transport's job and belongs in the service.
