// Package httpapi is the team's HTTP API standard as code: the response
// envelope, the error body, pagination, cache policy, ETags, request ids, the
// access log, panic recovery and the body limit.
//
// A survey of our services (2026-10-08) found three error bodies, three
// pagination shapes and Cache-Control written as string literals in handlers,
// across one gin and two echo services. Every one of them had re-solved the
// same problems, slightly differently. The standard is in team-playbook's
// conventional/RESTfulAPI.md; this package is what makes following it the
// path of least resistance.
//
// The logic lives here, on net/http types only. The echoapi and ginapi
// packages are thin adapters over it, and one conformance suite runs against
// both, so an echo service and a gin service answer byte-for-byte alike.
//
// # What this package does not do
//
// It does not route, bind or validate request bodies: the framework does that.
// It does not decide who may see a resource: authorization is the service's,
// and a cache policy only says how a response may be stored once it is sent.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("httpapi")

// The two values of an envelope's status field.
const (
	StatusSuccess = "success"
	StatusError   = "error"
)

// Envelope is the body of every successful response.
//
//	{"status":"success","data":{...}}
//	{"status":"success","message":"password changed"}
//	{"status":"success","message":"video published","data":{...}}
//	{"status":"success","data":[...],"metadata":{"offset":0,"limit":20,"total":53}}
//
// Message is optional text for a person, shown as-is by a client (a toast, a
// banner). A client never branches on it: what happened is the status code and
// the data. Message and Data are each omitted when empty, but a list's data is
// always present, as [] when it has no items.
type Envelope struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Data     any    `json:"data,omitempty"`
	Metadata any    `json:"metadata,omitempty"`
}

// Message is a success that has nothing to return but a sentence for a person.
// Prefer 204 when nobody reads the answer.
func Message(text string) Envelope { return Envelope{Status: StatusSuccess, Message: text} }

// WithMessage returns the envelope with a message for a person.
func (e Envelope) WithMessage(text string) Envelope {
	e.Message = text
	return e
}

// Metadata describes one page of an offset-paginated list.
type Metadata struct {
	Offset int   `json:"offset"`
	Limit  int   `json:"limit"`
	Total  int64 `json:"total"`
}

// CursorMetadata describes one page of a cursor-paginated list. NextCursor is
// empty on the last page.
type CursorMetadata struct {
	Limit      int    `json:"limit"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Success wraps data in the success envelope.
func Success(
	data any,
) Envelope {
	return Envelope{Status: StatusSuccess, Data: data}
}

// List wraps one page of items with its metadata. A nil slice is sent as an
// empty array, never null, so a client can always iterate.
func List[T any](items []T, metadata any) Envelope {
	if items == nil {
		items = []T{}
	}
	return Envelope{Status: StatusSuccess, Data: items, Metadata: metadata}
}

// WriteJSON writes v as the JSON body with status.
//
// The body is marshaled before the header is written, so a value that cannot
// be encoded becomes an error the caller can still answer, not a 200 with half
// a body.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return Error.Wrap(err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if status == http.StatusNotModified || status == http.StatusNoContent {
		return nil
	}
	_, err = w.Write(append(body, '\n'))
	return Error.Wrap(err)
}
