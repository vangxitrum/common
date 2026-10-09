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
	"bytes"
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
//	{"status":"success","data":{"videos":[...],"total":53},"query":{"offset":0,"limit":25}}
//
// Message is optional text for a person, shown as-is by a client (a toast, a
// banner). A client never branches on it: what happened is the status code and
// the data. Query is the request a list was actually served with, beside the
// data rather than inside it. Message, Data and Query are each omitted when
// empty.
type Envelope struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
	Query   any    `json:"query,omitempty"`
}

// Message is a success that has nothing to return but a sentence for a person.
// Prefer 204 when nobody reads the answer.
func Message(text string) Envelope { return Envelope{Status: StatusSuccess, Message: text} }

// WithMessage returns the envelope with a message for a person.
func (e Envelope) WithMessage(text string) Envelope {
	e.Message = text
	return e
}

// Success wraps data in the success envelope.
func Success(data any) Envelope { return Envelope{Status: StatusSuccess, Data: data} }

// List is one page of a collection: the items under the collection's own name
// and the total across every page in data, as aioz-stream answers, and beside
// it the query as it was actually served, with its defaults applied, so a
// caller sees the limit and sort it got without having sent them.
//
//	httpapi.List("videos", videos, total, page)
//	{"status":"success","data":{"videos":[...],"total":53},"query":{"offset":0,"limit":25}}
//
// query is usually the Page, or the endpoint's whole normalized request when
// it has filters; nil leaves it out. A nil slice is sent as [], never null, so
// a client can always iterate.
func List[T any](key string, items []T, total int64, query any) Envelope {
	if items == nil {
		items = []T{}
	}
	env := Success(listData{key: key, items: items, total: &total})
	env.Query = query
	return env
}

// CursorList is List for a cursor-paginated collection: no total, and the
// cursor of the next page, left out on the last one.
//
//	{"status":"success","data":{"events":[...],"next_cursor":"eyJpZCI6NDJ9"},"query":{"limit":25}}
func CursorList[T any](key string, items []T, nextCursor string, query any) Envelope {
	if items == nil {
		items = []T{}
	}
	env := Success(listData{key: key, items: items, nextCursor: nextCursor})
	env.Query = query
	return env
}

// listData writes its fields in a fixed order - the items, then the total or
// the next cursor - under a key only known at run time.
type listData struct {
	key        string
	items      any
	total      *int64
	nextCursor string
}

func (l listData) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	write := func(name string, v any) error {
		if buf.Len() > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(name)
		if err != nil {
			return err
		}
		val, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(val)
		return nil
	}
	if err := write(l.key, l.items); err != nil {
		return nil, err
	}
	if l.total != nil {
		if err := write("total", *l.total); err != nil {
			return nil, err
		}
	}
	if l.nextCursor != "" {
		if err := write("next_cursor", l.nextCursor); err != nil {
			return nil, err
		}
	}
	return append(append([]byte{'{'}, buf.Bytes()...), '}'), nil
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
