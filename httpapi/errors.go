package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
)

// ErrorBody is the body of every failed response.
//
//	{"status":"error","reason":"video-not-found","message":"no such video","request_id":"..."}
//
// Reason is the field a client branches on; Message is for a person. Details
// name the fields of a request that failed validation.
type ErrorBody struct {
	Status    string   `json:"status"`
	Reason    string   `json:"reason"`
	Message   string   `json:"message"`
	RequestID string   `json:"request_id,omitempty"`
	Details   []Detail `json:"details,omitempty"`
}

// Detail is one invalid field of a request.
type Detail struct {
	Field   string `json:"field"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// ReasonInternal is the reason of every error nobody classified.
const ReasonInternal = "internal"

// serverFaultMessage is what a client sees for any 5xx. A server fault's own
// message was written for the operator and routinely carries a driver error,
// a host or a query; the client gets the reason and the request id, which is
// what a support conversation needs.
const serverFaultMessage = "an unexpected error occurred"

type detailed struct {
	err     *apierr.Error
	details []Detail
}

func (d *detailed) Error() string { return d.err.Error() }
func (d *detailed) Unwrap() error { return d.err }

// WithDetails attaches the invalid fields to a validation error, for a 400 or
// 422 that names what to fix.
func WithDetails(err *apierr.Error, details ...Detail) error {
	return &detailed{err: err, details: details}
}

// FromStatus classifies a status a framework produced on its own - an
// unrouted path, a method with no handler, a body over the limit - so it is
// rendered like any other apierr.
func FromStatus(status int, message string) *apierr.Error {
	reason := map[int]string{
		http.StatusBadRequest:            "bad-request",
		http.StatusUnauthorized:          "unauthorized",
		http.StatusForbidden:             "forbidden",
		http.StatusNotFound:              "route-not-found",
		http.StatusMethodNotAllowed:      "method-not-allowed",
		http.StatusRequestEntityTooLarge: "body-too-large",
		http.StatusUnsupportedMediaType:  "unsupported-media-type",
		http.StatusTooManyRequests:       "too-many-requests",
		http.StatusServiceUnavailable:    "unavailable",
	}[status]
	if reason == "" {
		reason = ReasonInternal
		if status < http.StatusInternalServerError {
			reason = "bad-request"
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return apierr.New(reason, status, "%s", message)
}

// Render decides the status and body for err.
//
// An apierr is rendered as it asked, with its client message and never its
// cause. A body over the limit is a 413 however deep the framework wrapped it.
// Anything else is a 500: a surprise is a server fault until someone says
// otherwise.
func Render(err error, requestID string) (int, ErrorBody) {
	body := ErrorBody{Status: StatusError, RequestID: requestID}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		err = FromStatus(
			http.StatusRequestEntityTooLarge,
			"request body too large",
		)
	}

	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		body.Reason = ReasonInternal
		body.Message = serverFaultMessage
		return http.StatusInternalServerError, body
	}

	status := apiErr.Status()
	body.Reason = apiErr.Reason()
	if body.Reason == "" {
		body.Reason = ReasonInternal
	}
	body.Message = apiErr.ClientMessage()
	if body.Message == "" {
		body.Message = http.StatusText(status)
	}
	if status >= http.StatusInternalServerError {
		body.Message = serverFaultMessage
	}

	var withDetails *detailed
	if errors.As(err, &withDetails) {
		body.Details = withDetails.details
	}
	return status, body
}

// WriteError renders err and writes it. An error response is never stored by
// any cache and carries no validator: a 404 cached for five minutes under a
// route's public policy outlives the resource being created.
func WriteError(w http.ResponseWriter, err error, requestID string) {
	status, body := Render(err, requestID)
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Del("ETag")
	h.Del("Last-Modified")
	if status == http.StatusTooManyRequests && h.Get("Retry-After") == "" {
		h.Set("Retry-After", "1")
	}
	_ = WriteJSON(w, status, body)
}

// LogError logs a server fault with everything an operator needs to place it:
// the reason, the packages it passed through, the stack where it started and
// whatever the service knew about the subject. A client error is not logged
// here; the access log already has it.
func LogError(
	ctx context.Context,
	log *slog.Logger,
	err error,
	route, requestID string,
) {
	status, body := Render(err, requestID)
	if status < http.StatusInternalServerError || log == nil {
		return
	}
	fields := apierr.Fields(err)
	attrs := make([]any, 0, 12+len(fields))
	attrs = append(attrs,
		"route", route,
		"request_id", requestID,
		"reason", body.Reason,
		"err", err.Error(),
		"tags", apierr.Tags(err),
		"stack", apierr.Trace(err),
	)
	attrs = append(attrs, fields...)
	log.ErrorContext(ctx, "request failed", attrs...)
}
