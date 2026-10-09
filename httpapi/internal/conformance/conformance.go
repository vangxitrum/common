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
//	GET  /v1/items       Page(PageConfig{}); two items {"id":"a"},{"id":"b"}
//	                     as List("items", items, 53, page)
//	GET  /v1/cached      Cache(Revalidate); CheckETag(ETag("v1")) or OK({"v":1})
//	GET  /v1/public      Cache(Public, MaxAge 5m); OK({"v":1})
//	GET  /v1/public-fail Cache(Public, MaxAge 5m); apierr.Conflict("busy", "try later")
//	GET  /v1/plain       errors.New("db down: host 10.0.0.5")
//	GET  /v1/panic       panic("boom")
//	GET  /v1/limited     apierr.TooManyRequests("slow-down", "too many requests")
//	POST /v1/password    Send(200, Message("password changed"))
//	POST /v1/publish     Send(201, Success({"id":"v1"}).WithMessage("video published"))
//	POST /v1/echo        Bind into struct{Name string `json:"name"`}; Created(it)
//	POST /v1/lenient     the same, with Bind(..., httpapi.AllowUnknownFields())
//	POST /v1/upload      BodyLimit(UploadLimit); read the whole body and
//	                     OK({"bytes": n}), or return the read error
//	POST /v1/webhook     BodyLimit(WebhookLimit); the same as /v1/echo
//	GET  /health         OK({"ok": true})
//	GET  /docs           ContentSecurityPolicy(httpsec.SwaggerUIPolicy); OK({"docs": true})
//
// and passes Options to Setup.
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

// UploadLimit is the upload route's body limit, above the stack's, which a
// route must be able to raise.
const UploadLimit = 4 * BodyLimit

// WebhookLimit is the webhook route's body limit, below the stack's.
const WebhookLimit = 16

// Options are the Setup options every adapter under test is built with: the
// health check stays out of the access log, and so does /v1/plain unless it
// fails, which it always does.
var Options = []httpapi.Option{httpapi.SkipLog("/health", "/v1/plain")}

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
	// unsized sends the body with no Content-Length, as a chunked upload is,
	// so only reading it can find it too large.
	unsized bool
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
				"Content-Type":            errorCT,
				"Cache-Control":           "no-store",
				"X-Content-Type-Options":  "nosniff",
				"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
			},
			// HSTS is opt-in in httpsec, and this stack leaves it off.
			absent: []string{"Strict-Transport-Security"},
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
			body:   `{"status":"error","reason":"item-not-found","message":"no such item","request_id":"<generated>"}`,
			headers: map[string]string{
				"Cache-Control": "no-store",
			},
		},
	},
	{
		"a caller's request id is never used, however sane",
		request{
			method:  "GET",
			path:    "/v1/items/1",
			headers: map[string]string{"X-Request-Id": "trace-1"},
		},
		expect{
			status: 200,
		},
	},
	{
		"a list is the items under their name, the total and the query",
		request{method: "GET", path: "/v1/items?offset=10&limit=2"},
		expect{
			status: 200,
			body:   `{"status":"success","data":{"items":[{"id":"a"},{"id":"b"}],"total":53},"query":{"offset":10,"limit":2}}`,
		},
	},
	{
		"no limit is the default of 25",
		request{method: "GET", path: "/v1/items"},
		expect{
			status: 200,
			body:   `{"status":"success","data":{"items":[{"id":"a"},{"id":"b"}],"total":53},"query":{"offset":0,"limit":25}}`,
		},
	},
	{
		"a limit over the maximum is refused",
		request{
			method:  "GET",
			path:    "/v1/items?limit=1000",
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 400,
			body:   `{"status":"error","reason":"limit-too-large","message":"limit must be at most 100","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"offset-with-cursor","message":"use offset or cursor, not both","request_id":"<generated>"}`,
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
			body:    `{"status":"error","reason":"busy","message":"try later","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"internal","message":"an unexpected error occurred","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"panic","message":"an unexpected error occurred","request_id":"<generated>"}`,
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
			body:    `{"status":"error","reason":"slow-down","message":"too many requests","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"route-not-found","message":"Not Found","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"method-not-allowed","message":"Method Not Allowed","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"unknown-field","message":"request body has a field this API does not accept","request_id":"<generated>","details":[{"field":"nam","reason":"unknown-field"}]}`,
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
			body:   `{"status":"error","reason":"invalid-json","message":"request body is not valid JSON for this endpoint","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"unsupported-media-type","message":"request body must be application/json","request_id":"<generated>"}`,
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
			body:   `{"status":"error","reason":"body-too-large","message":"request body too large","request_id":"<generated>"}`,
		},
	},
	{
		"an unknown field is ignored where the route allows it",
		request{
			method:  "POST",
			path:    "/v1/lenient",
			body:    `{"nam":"x","name":"y"}`,
			headers: map[string]string{"Content-Type": "application/json"},
		},
		expect{
			status: 201,
			body:   `{"status":"success","data":{"name":"y"}}`,
		},
	},
	{
		"a route can raise the body limit above the stack's",
		request{
			method: "POST",
			path:   "/v1/upload",
			body:   strings.Repeat("x", 2*BodyLimit),
		},
		expect{
			status: 200,
			body:   fmt.Sprintf(`{"status":"success","data":{"bytes":%d}}`, 2*BodyLimit),
		},
	},
	{
		"a declared body over a raised limit is a 413",
		request{
			method:  "POST",
			path:    "/v1/upload",
			body:    strings.Repeat("x", UploadLimit+1),
			headers: map[string]string{"X-Request-Id": "r"},
		},
		expect{
			status: 413,
			body:   `{"status":"error","reason":"body-too-large","message":"request body too large","request_id":"<generated>"}`,
		},
	},
	{
		"an undeclared body over a raised limit is a 413",
		request{
			method:  "POST",
			path:    "/v1/upload",
			body:    strings.Repeat("x", UploadLimit+1),
			headers: map[string]string{"X-Request-Id": "r"},
			unsized: true,
		},
		expect{
			status: 413,
			body:   `{"status":"error","reason":"body-too-large","message":"request body too large","request_id":"<generated>"}`,
		},
	},
	{
		"a route can lower the body limit below the stack's",
		request{
			method: "POST",
			path:   "/v1/webhook",
			body:   `{"name":"` + strings.Repeat("x", WebhookLimit) + `"}`,
			headers: map[string]string{
				"Content-Type": "application/json",
				"X-Request-Id": "r",
			},
		},
		expect{
			status: 413,
			body:   `{"status":"error","reason":"body-too-large","message":"request body too large","request_id":"<generated>"}`,
		},
	},
	{
		"a body within a lowered limit",
		request{
			method:  "POST",
			path:    "/v1/webhook",
			body:    `{"name":"x"}`,
			headers: map[string]string{"Content-Type": "application/json"},
		},
		expect{
			status: 201,
			body:   `{"status":"success","data":{"name":"x"}}`,
		},
	},
	{
		"a page on an allowed origin can read the request id",
		request{
			method:  "GET",
			path:    "/v1/items/1",
			headers: map[string]string{"Origin": Origin},
		},
		expect{
			status: 200,
			headers: map[string]string{
				"Access-Control-Allow-Origin":   Origin,
				"Access-Control-Expose-Headers": "X-Request-Id",
			},
		},
	},
	{
		"an error a page reads also exposes the request id",
		request{
			method:  "GET",
			path:    "/v1/items/missing",
			headers: map[string]string{"Origin": Origin},
		},
		expect{
			status:  404,
			headers: map[string]string{"Access-Control-Expose-Headers": "X-Request-Id"},
		},
	},
	{
		"another origin is told nothing",
		request{
			method:  "GET",
			path:    "/v1/items/1",
			headers: map[string]string{"Origin": "https://evil.example"},
		},
		expect{
			status: 200,
			absent: []string{"Access-Control-Allow-Origin", "Access-Control-Expose-Headers"},
		},
	},
	{
		"a docs route has its own content security policy",
		request{method: "GET", path: "/docs"},
		expect{
			status: 200,
			headers: map[string]string{
				"Content-Security-Policy": httpsec.SwaggerUIPolicy,
				"X-Frame-Options":         "DENY",
			},
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
			if c.req.unsized {
				r.ContentLength = -1
			}
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
			if len(id) != 36 {
				t.Errorf("request id %q, want a UUID the server made", id)
			}
			if sent := c.req.headers["X-Request-Id"]; sent != "" && id == sent {
				t.Errorf("the caller's request id %q was used", sent)
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
				// "<generated>" stands for the id the server made, which
				// the check above already holds to the header's.
				if m, ok := want.(map[string]any); ok && m["request_id"] == "<generated>" {
					m["request_id"] = id
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
			map[string]any{"group": "api", "route": "/v1/items/:id"},
			[]string{"reason", "err"}},
		{"unrouted", "GET", "/v1/nothing-here", "log-2",
			"\x1b[94mGET\x1b[0m status:\x1b[33m404\x1b[0m latency:", " url:/v1/nothing-here",
			map[string]any{"group": "api", "reason": "route-not-found"},
			[]string{"err"}},
		{"server fault", "GET", "/v1/plain", "log-3",
			"\x1b[94mGET\x1b[0m status:\x1b[31m500\x1b[0m latency:", " url:/v1/plain",
			map[string]any{"group": "api", "reason": "internal", "err": "db down: host 10.0.0.5"},
			nil},
		{"three-letter method", "POST", "/v1/password", "log-4",
			"\x1b[92mPST\x1b[0m status:\x1b[32m200\x1b[0m latency:", " url:/v1/password",
			map[string]any{"group": "api"},
			nil},
	} {
		t.Run("log "+c.name, func(t *testing.T) {
			logs.records(t) // drop anything earlier
			r := httptest.NewRequest(c.method, c.path, nil)
			r.Header.Set("X-Request-Id", c.reqID)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			id := w.Header().Get(httpapi.RequestIDHeader)

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
			if access["request_id"] != id || id == c.reqID {
				t.Errorf("request_id %v, want the response's %q and never the caller's %q",
					access["request_id"], id, c.reqID)
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

	t.Run("log a skipped path that succeeds is left out", func(t *testing.T) {
		logs.records(t)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
		for _, rec := range logs.records(t) {
			if rec["group"] == "api" {
				t.Fatalf("access record for a skipped path: %v", rec)
			}
		}
	})
}

// RunWithout checks a stack set up with httpapi.WithoutAccessLog() and
// httpapi.WithoutRecovery(), on the routes GET /v1/items/:id and GET
// /v1/panic as Run has them: nothing writes an access record, a panic reaches
// the caller for the service's own recovery, and the response contract is
// unchanged.
func RunWithout(t *testing.T, h http.Handler, logs *Logs) {
	t.Helper()

	t.Run("without the access log", func(t *testing.T) {
		logs.records(t)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/items/42", nil))
		if w.Code != 200 || w.Header().Get(httpapi.RequestIDHeader) == "" {
			t.Fatalf("status %d, request id %q", w.Code, w.Header().Get(httpapi.RequestIDHeader))
		}
		for _, rec := range logs.records(t) {
			if rec["group"] == "api" {
				t.Fatalf("access record with the access log off: %v", rec)
			}
		}
	})

	t.Run("without recovery", func(t *testing.T) {
		defer func() {
			if v := recover(); v != "boom" {
				t.Fatalf("recovered %v, want the handler's panic", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/panic", nil))
		t.Fatal("the panic was recovered with recovery off")
	})
}
