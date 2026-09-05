// Package apierr models errors that cross the HTTP boundary.
//
// An apierr carries three things a caller needs and a Go error alone does not:
// the HTTP status to answer with, a machine-readable reason slug the client
// can branch on without parsing prose, and the underlying error for the log.
// The transport renders the first two into the JSON envelope; nothing else has
// to know how a given failure maps to a status code.
//
// The convention for a reason, borrowed from depin's grpcerr, is lowercase
// kebab-case naming the cause rather than the code: "name-too-long", not
// "bad-request". The status can change without breaking a client that
// branches on the reason.
//
// A slice returns these from its service; wrapping is preserved, so
// errors.Is and errors.As keep working through them.
package apierr

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is an error with an HTTP status, a reason slug and optional structured
// fields naming the subject.
type Error struct {
	reason string
	status int
	msg    string
	err    error
	fields []any
}

// Error implements error.
func (e *Error) Error() string {
	if e.err != nil && e.msg == "" {
		return e.err.Error()
	}
	if e.err != nil {
		return e.msg + ": " + e.err.Error()
	}
	return e.msg
}

// Unwrap exposes the wrapped error to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.err }

// Reason returns the machine-readable slug.
func (e *Error) Reason() string { return e.reason }

// Fields returns the structured key/value pairs describing the subject, in
// slog's argument form. They go to the log, never to the client: they name
// concrete records and must not leak into an API response.
func (e *Error) Fields() []any { return e.fields }

// With attaches key/value pairs naming the subject of the failure - which
// record, which id. This is the axis depin's error model lacks: with only a
// package tag and a status tag, "contract not found" inside the file service
// renders as "file: not found", because nothing carries the subject.
//
// Arguments are slog-style pairs:
//
//	apierr.NotFound("contract-not-found", "no such contract").
//	    With("resource", "contract", "id", contractID)
func (e *Error) With(kv ...any) *Error {
	e.fields = append(e.fields, kv...)
	return e
}

// Status returns the HTTP status.
func (e *Error) Status() int { return e.status }

// New returns an Error with no wrapped cause.
func New(reason string, status int, format string, a ...any) *Error {
	return &Error{reason: reason, status: status, msg: fmt.Sprintf(format, a...)}
}

// Wrap turns err into an Error, keeping it unwrappable. It returns nil when
// err is nil, so it is safe on a bare return path.
func Wrap(reason string, status int, err error) error {
	if err == nil {
		return nil
	}
	return &Error{reason: reason, status: status, err: err}
}

// WrapWith is Wrap plus subject fields, for the common case where the caller
// knows which record failed.
//
// It returns error rather than *Error on purpose. The wrap constructors can
// legitimately receive a nil cause, and a nil *Error returned into an error
// variable is not nil - the interface holds a type. Only the New-family
// constructors, which can never produce nil, return *Error so that With
// chains.
func WrapWith(reason string, status int, err error, kv ...any) error {
	if err == nil {
		return nil
	}
	return (&Error{reason: reason, status: status, err: err}).With(kv...)
}

// Wrapf is Wrap with a message of its own in front of the cause.
func Wrapf(reason string, status int, err error, format string, a ...any) error {
	if err == nil {
		return nil
	}
	return &Error{reason: reason, status: status, msg: fmt.Sprintf(format, a...), err: err}
}

// Status reports the HTTP status err asks for, defaulting to 500. An error
// that is not an apierr is by definition one nobody classified, and a
// surprise is a server fault until someone says otherwise.
func Status(err error) int {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.status
	}
	return http.StatusInternalServerError
}

// Reason reports the slug err carries, or "" when it has none.
func Reason(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.reason
	}
	return ""
}

// Fields reports the subject fields err carries, or nil when it has none.
func Fields(err error) []any {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.fields
	}
	return nil
}

// Is reports whether err is an apierr answering with status.
func Is(err error, status int) bool { return Status(err) == status }

// The constructors below cover the statuses a service actually returns.
// Forbidden is here deliberately: depin's own error mapping omitted it once,
// so authorization failures answered as Internal - a 500 for what was really
// a 403, which reads as an outage rather than a permission problem.

// BadRequest is 400: the client sent something unusable.
func BadRequest(reason, format string, a ...any) *Error {
	return New(reason, http.StatusBadRequest, format, a...)
}

// Unauthorized is 401: the client did not prove who it is.
func Unauthorized(reason, format string, a ...any) *Error {
	return New(reason, http.StatusUnauthorized, format, a...)
}

// Forbidden is 403: the client is known but not allowed.
func Forbidden(reason, format string, a ...any) *Error {
	return New(reason, http.StatusForbidden, format, a...)
}

// NotFound is 404.
func NotFound(reason, format string, a ...any) *Error {
	return New(reason, http.StatusNotFound, format, a...)
}

// Conflict is 409: the request fights the current state.
func Conflict(reason, format string, a ...any) *Error {
	return New(reason, http.StatusConflict, format, a...)
}

// TooManyRequests is 429.
func TooManyRequests(reason, format string, a ...any) *Error {
	return New(reason, http.StatusTooManyRequests, format, a...)
}

// Internal is 500, for a fault the client can do nothing about.
func Internal(reason string, err error, kv ...any) error {
	return WrapWith(reason, http.StatusInternalServerError, err, kv...)
}

// Unavailable is 503, for a dependency that is down or a service still
// starting up.
func Unavailable(reason, format string, a ...any) *Error {
	return New(reason, http.StatusServiceUnavailable, format, a...)
}
