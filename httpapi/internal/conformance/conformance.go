// Package conformance is the one test suite every httpapi adapter passes, so
// an echo service and a gin service answer the same request with the same
// status, headers and bytes.
//
// An adapter's test builds a handler with NewStack and these routes, each
// written the way a service would write it with that adapter, and hands it to
// Run:
//
//	GET  /v1/items/:id   id "missing": apierr.NotFound("item-not-found", "no such item");
//	                     else OK({"id": id})
//	GET  /v1/items       Page(PageConfig{}); List of two items {"id":"a"},{"id":"b"}
//	                     with page.Metadata(53)
//	GET  /v1/cached      Cache(Revalidate); CheckETag(ETag("v1")) or OK({"v":1})
//	GET  /v1/public      Cache(Public, MaxAge 5m); OK({"v":1})
//	GET  /v1/public-fail Cache(Public, MaxAge 5m); apierr.Conflict("busy", "try later")
//	GET  /v1/plain       errors.New("db down: host 10.0.0.5")
//	GET  /v1/panic       panic("boom")
//	GET  /v1/limited     apierr.TooManyRequests("slow-down", "too many requests")
//	POST /v1/password    Send(200, Message("password changed"))
//	POST /v1/publish     Send(201, Success({"id":"v1"}).WithMessage("video published"))
//	POST /v1/echo        Bind into struct{Name string `json:"name"`}; Created(it)
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

// Origin is the one origin the suite's CORS policy allows.
const Origin = "https://app.example.com"

// BodyLimit is the suite's body limit, small enough to cross in a test.
const BodyLimit = 64

// Stack is the stack every adapter under test is built with.
func Stack(t *testing.T) *httpapi.Stack {
	t.Helper()
	stack, err := httpapi.NewStack(httpapi.Config{
		BodyLimit: BodyLimit,
		CORS:      httpsec.CORSConfig{AllowedOrigins: []string{Origin}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return stack
}

// Public is the policy the suite's public routes use.
var Public = httpapi.CachePolicy{Type: httpapi.Public, MaxAge: 5 * time.Minute}

type request struct {
	method  string
	path    string
	body    string
	headers map[string]string
}

type expect struct {
	status  int
	body    string // exact JSON, compared semantically; "" means no body
	headers map[string]string
	absent  []string
	vary    []string // values Vary must list
	novary  []string // values Vary must not list
}

type check struct {
	name string
	req  request
	want expect
}

const errorCT = "application/json; charset=utf-8"

var checks = []check{
	{
		"a success with only a message",
		request{method: "POST", path: "/v1/password"},
		expect{
			status: 200,
			body:   `{"status":"success","message":"password changed"}`,
		},
	},
	{
		"a success with a message and data",
		request{method: "POST", path: "/v1/publish"},
		expect{
			status: 201,
			body:   `{"status":"success","message":"video published","data":{"id":"v1"}}`,
		},
	},
	{
		"a generated request id is the same in the header and the body",
		request{method: "GET", path: "/v1/items/missing"},
		expect{
			status: 404,
		},
	},
	{
		"success envelope",
		request{method: "GET", path: "/v1/items/42"},
		expect{
			status: 200,
			body:   `{"status":"success","data":{"id":"42"}}`,
			headers: map[string]string{
				"Content-Type":           errorCT,
				"Cache-Control":          "no-store",
				"X-Content-Type-Options": "nosniff",
			},
		},
	},
	{
		"apierr renders its reason and client message",
		request{
			method:  "GET",
			path:    "/v1/items/missing",
			headers: map[string]string{"X-Request-Id": "req-1"},
		},
		expect{
			status: 404,
			body:   `{"status":"error","reason":"item-not-found","message":"no such item","request_id":"req-1"}`,
			headers: map[string]string{
				"X-Request-Id":  "req-1",
				"Cache-Control": "no-store",
			},
		},
	},
	{
		"a caller's unsafe request id is replaced",
		request{
			method:  "GET",
			path:    "/v1/items/1",
			headers: map[string]string{"X-Request-Id": "bad id\n"},
		},
		expect{
			status: 200,
		},
	},
	{
		"list with metadata",
		request{method: "GET", path: "/v1/items?offset=10&limit=2"},
		expect{
			status: 200,
			body:   `{"status":"success","data":[{"id":"a"},{"id":"b"}],"metadata":{"offset":10,"limit":2,"total":53}}`,
		},
	},
	{
		"limit over the maximum is refused",
		request{
			method:  "GET",
			path:    "/v1/items?limit=1000",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 400,
			body:   `{"status":"error","reason":"limit-too-large","message":"limit must be at most 100","request_id":"r"}`,
		},
	},
	{
		"offset with cursor is refused",
		request{
			method:  "GET",
			path:    "/v1/items?offset=1&cursor=YQ",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 400,
			body:   `{"status":"error","reason":"offset-with-cursor","message":"use offset or cursor, not both","request_id":"r"}`,
		},
	},
	{
		"revalidate sends the policy and the ETag",
		request{method: "GET", path: "/v1/cached"},
		expect{
			status: 200,
			body:   `{"status":"success","data":{"v":1}}`,
			headers: map[string]string{
				"Cache-Control": "private, no-cache",
				"ETag":          `"v1"`,
			},
			vary: []string{"Authorization"},
		},
	},
	{
		"a matching If-None-Match is a 304 with no body",
		request{
			method:  "GET",
			path:    "/v1/cached",
			headers: map[string]string{"If-None-Match": `W/"v0", "v1"`},
		},
		expect{
			status: 304,
			headers: map[string]string{
				"Cache-Control": "private, no-cache",
				"ETag":          `"v1"`,
			},
		},
	},
	{"public policy", request{method: "GET", path: "/v1/public"}, expect{
		status:  200,
		headers: map[string]string{"Cache-Control": "public, max-age=300"},
		novary:  []string{"Authorization"},
	}},
	{
		"an error on a public route is never cached",
		request{
			method:  "GET",
			path:    "/v1/public-fail",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status:  409,
			body:    `{"status":"error","reason":"busy","message":"try later","request_id":"r"}`,
			headers: map[string]string{"Cache-Control": "no-store"},
		},
	},
	{
		"an unclassified error is a 500 that leaks nothing",
		request{
			method:  "GET",
			path:    "/v1/plain",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 500,
			body:   `{"status":"error","reason":"internal","message":"an unexpected error occurred","request_id":"r"}`,
		},
	},
	{
		"a panic is a 500 and the process lives",
		request{
			method:  "GET",
			path:    "/v1/panic",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 500,
			body:   `{"status":"error","reason":"panic","message":"an unexpected error occurred","request_id":"r"}`,
		},
	},
	{
		"429 carries Retry-After",
		request{
			method:  "GET",
			path:    "/v1/limited",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status:  429,
			body:    `{"status":"error","reason":"slow-down","message":"too many requests","request_id":"r"}`,
			headers: map[string]string{"Retry-After": "1"},
		},
	},
	{
		"an unrouted path",
		request{
			method:  "GET",
			path:    "/v1/nothing-here",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 404,
			body:   `{"status":"error","reason":"route-not-found","message":"Not Found","request_id":"r"}`,
		},
	},
	{
		"a method with no handler",
		request{
			method:  "DELETE",
			path:    "/v1/public",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 405,
			body:   `{"status":"error","reason":"method-not-allowed","message":"Method Not Allowed","request_id":"r"}`,
		},
	},
	{
		"a valid body",
		request{
			method:  "POST",
			path:    "/v1/echo",
			body:    `{"name":"x"}`,
			headers: map[string]string{"Content-Type": "application/json"},
		},
		expect{
			status: 201,
			body:   `{"status":"success","data":{"name":"x"}}`,
		},
	},
	{
		"an unknown field names itself",
		request{
			method: "POST",
			path:   "/v1/echo",
			body:   `{"nam":"x"}`,
			headers: map[string]string{
				"Content-Type": "application/json",
				"X-Request-Id": "r",
			},
		},
		expect{
			status: 400,
			body:   `{"status":"error","reason":"unknown-field","message":"request body has a field this API does not accept","request_id":"r","details":[{"field":"nam","reason":"unknown-field"}]}`,
		},
	},
	{
		"invalid JSON does not quote the parser",
		request{
			method: "POST",
			path:   "/v1/echo",
			body:   `{"name":`,
			headers: map[string]string{
				"Content-Type": "application/json",
				"X-Request-Id": "r",
			},
		},
		expect{
			status: 400,
			body:   `{"status":"error","reason":"invalid-json","message":"request body is not valid JSON for this endpoint","request_id":"r"}`,
		},
	},
	{
		"a body that is not JSON is a 415",
		request{
			method: "POST",
			path:   "/v1/echo",
			body:   `name=x`,
			headers: map[string]string{
				"Content-Type": "application/x-www-form-urlencoded",
				"X-Request-Id": "r",
			},
		},
		expect{
			status: 415,
			body:   `{"status":"error","reason":"unsupported-media-type","message":"request body must be application/json","request_id":"r"}`,
		},
	},
	{
		"a body over the limit is a 413",
		request{
			method: "POST",
			path:   "/v1/echo",
			body:   `{"name":"` + strings.Repeat("x", BodyLimit) + `"}`,
			headers: map[string]string{
				"Content-Type": "application/json",
				"X-Request-Id": "r",
			},
		},
		expect{
			status: 413,
			body:   `{"status":"error","reason":"body-too-large","message":"request body too large","request_id":"r"}`,
		},
	},
	{
		"a preflight from an allowed origin",
		request{
			method: "OPTIONS",
			path:   "/v1/items/1",
			headers: map[string]string{
				"Origin":                        Origin,
				"Access-Control-Request-Method": "GET",
			},
		},
		expect{
			status:  204,
			headers: map[string]string{"Access-Control-Allow-Origin": Origin},
		},
	},
}

// Run sends every check to h and compares the answer.
func Run(t *testing.T, h http.Handler) {
	t.Helper()
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			var body io.Reader
			if c.req.body != "" {
				body = strings.NewReader(c.req.body)
			}
			r := httptest.NewRequest(c.req.method, c.req.path, body)
			for k, v := range c.req.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != c.want.status {
				t.Fatalf(
					"status %d, want %d; body %s",
					w.Code,
					c.want.status,
					w.Body.String(),
				)
			}
			for k, v := range c.want.headers {
				if got := w.Header().Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
			for _, k := range c.want.absent {
				if got := w.Header().Get(k); got != "" {
					t.Errorf("header %s = %q, want none", k, got)
				}
			}
			vary := strings.Join(w.Header().Values("Vary"), ",")
			for _, v := range c.want.vary {
				if !strings.Contains(vary, v) {
					t.Errorf("Vary %q does not list %s", vary, v)
				}
			}
			for _, v := range c.want.novary {
				if strings.Contains(vary, v) {
					t.Errorf("Vary %q lists %s", vary, v)
				}
			}
			id := w.Header().Get(httpapi.RequestIDHeader)
			if id == "" || strings.ContainsAny(id, " \n") {
				t.Errorf("request id %q", id)
			}

			switch {
			case c.want.body == "" && (c.want.status == 304 || c.want.status == 204):
				if w.Body.Len() != 0 {
					t.Errorf("body %q, want none", w.Body.String())
				}
			case c.want.body != "":
				var got, want any
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("body %q is not JSON: %v", w.Body.String(), err)
				}
				if err := json.Unmarshal(
					[]byte(c.want.body),
					&want,
				); err != nil {
					t.Fatal(err)
				}
				if m, ok := got.(map[string]any); ok &&
					m["request_id"] != nil &&
					m["request_id"] != id {
					t.Errorf(
						"body request_id %v, header %q",
						m["request_id"],
						id,
					)
				}
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("body\n got %s\nwant %s", gotJSON, wantJSON)
				}
			}
		})
	}
}

// Logs is where an adapter under test writes its logs, for RunLogs.
type Logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *Logs) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	l.buf.Reset()
	return out
}

// Logger is the logger an adapter under test is set up with.
func Logger(logs *Logs) *slog.Logger { return slog.New(slog.NewJSONHandler(logs, nil)) }

// RunLogs checks the access log line: aioz-stream's request line in group
// "api", the same on every adapter, with the query string never logged.
func RunLogs(t *testing.T, h http.Handler, logs *Logs) {
	t.Helper()
	for _, c := range []struct {
		name, method, path, reqID string
		msgPrefix, msgSuffix      string
		attrs                     map[string]any
		absent                    []string
	}{
		{"success", "GET", "/v1/items/42?token=secret", "log-1",
			"\x1b[94mGET\x1b[0m status:\x1b[32m200\x1b[0m latency:", " url:/v1/items/42",
			map[string]any{"group": "api", "request_id": "log-1", "route": "/v1/items/:id"},
			[]string{"reason", "err"}},
		{"unrouted", "GET", "/v1/nothing-here", "log-2",
			"\x1b[94mGET\x1b[0m status:\x1b[33m404\x1b[0m latency:", " url:/v1/nothing-here",
			map[string]any{"group": "api", "request_id": "log-2", "reason": "route-not-found"},
			[]string{"err"}},
		{"server fault", "GET", "/v1/plain", "log-3",
			"\x1b[94mGET\x1b[0m status:\x1b[31m500\x1b[0m latency:", " url:/v1/plain",
			map[string]any{"group": "api", "request_id": "log-3", "reason": "internal", "err": "db down: host 10.0.0.5"},
			nil},
		{"three-letter method", "POST", "/v1/password", "log-4",
			"\x1b[92mPST\x1b[0m status:\x1b[32m200\x1b[0m latency:", " url:/v1/password",
			map[string]any{"group": "api", "request_id": "log-4"},
			nil},
	} {
		t.Run("log "+c.name, func(t *testing.T) {
			logs.records(t) // drop anything earlier
			r := httptest.NewRequest(c.method, c.path, nil)
			r.Header.Set("X-Request-Id", c.reqID)
			h.ServeHTTP(httptest.NewRecorder(), r)

			var access map[string]any
			for _, rec := range logs.records(t) {
				if rec["group"] == "api" {
					access = rec
				}
			}
			if access == nil {
				t.Fatal("no api-group record")
			}
			msg, _ := access["msg"].(string)
			if !strings.HasPrefix(msg, c.msgPrefix) || !strings.HasSuffix(msg, c.msgSuffix) {
				t.Errorf("msg %q, want %q...%q", msg, c.msgPrefix, c.msgSuffix)
			}
			if access["level"] != "INFO" {
				t.Errorf("level %v", access["level"])
			}
			for k, want := range c.attrs {
				if access[k] != want {
					t.Errorf("%s = %v, want %v", k, access[k], want)
				}
			}
			for _, k := range c.absent {
				if v, ok := access[k]; ok {
					t.Errorf("%s = %v, want none", k, v)
				}
			}
			if strings.Contains(fmt.Sprint(access), "secret") {
				t.Error("the query string was logged")
			}
		})
	}
}
