package httpapi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
)

func TestCacheControl(t *testing.T) {
	for _, tc := range []struct {
		policy httpapi.CachePolicy
		want   string
		vary   bool
	}{
		{httpapi.CachePolicy{Type: httpapi.Public, MaxAge: time.Hour}, "public, max-age=3600", false},
		{httpapi.CachePolicy{Type: httpapi.Public, MaxAge: time.Minute, StaleWhileRevalidate: 30 * time.Second}, "public, max-age=60, stale-while-revalidate=30", false},
		{httpapi.CachePolicy{Type: httpapi.Public}, "public, no-cache", false},
		{httpapi.CachePolicy{Type: httpapi.Private, MaxAge: time.Minute}, "private, max-age=60", true},
		{httpapi.CachePolicy{Type: httpapi.Revalidate}, "private, no-cache", true},
		{httpapi.CachePolicy{Type: httpapi.NoStore}, "no-store", false},
	} {
		if err := tc.policy.Validate(); err != nil {
			t.Fatalf("%+v: %v", tc.policy, err)
		}
		h := http.Header{}
		h.Set("Cache-Control", "no-store") // what httpsec sent first
		tc.policy.Apply(h)
		if got := h.Get("Cache-Control"); got != tc.want {
			t.Errorf(
				"%s: Cache-Control %q, want %q",
				tc.policy.Type,
				got,
				tc.want,
			)
		}
		if got := h.Get("Vary") == "Authorization"; got != tc.vary {
			t.Errorf(
				"%s: Vary Authorization %v, want %v",
				tc.policy.Type,
				got,
				tc.vary,
			)
		}
	}
}

func TestInvalidCachePolicies(t *testing.T) {
	for _, p := range []httpapi.CachePolicy{
		{},
		{Type: "forever"},
		{Type: httpapi.NoStore, MaxAge: time.Minute},
		{Type: httpapi.Revalidate, MaxAge: time.Minute},
		{Type: httpapi.Private, StaleWhileRevalidate: time.Minute},
		{Type: httpapi.Public, MaxAge: -time.Second},
	} {
		if p.Validate() == nil {
			t.Errorf("%+v validated", p)
		}
	}
}

func TestETag(t *testing.T) {
	if got := httpapi.ETag("v42"); got != `"v42"` {
		t.Errorf("ETag %s", got)
	}
	if got := httpapi.WeakETag("v42"); got != `W/"v42"` {
		t.Errorf("WeakETag %s", got)
	}
	unsafe := httpapi.ETag(`has "quotes" and spaces`)
	if strings.Count(unsafe, `"`) != 2 || strings.Contains(unsafe, " ") {
		t.Errorf("unsafe version not hashed: %s", unsafe)
	}
}

func TestNotModified(t *testing.T) {
	for _, tc := range []struct {
		method, header, etag string
		want                 bool
	}{
		{"GET", `"a"`, `"a"`, true},
		{"HEAD", `"a"`, `"a"`, true},
		{"GET", `W/"a"`, `"a"`, true}, // weak comparison
		{"GET", `"x", W/"a"`, `"a"`, true},
		{"GET", `*`, `"a"`, true},
		{"GET", `"b"`, `"a"`, false},
		{"GET", ``, `"a"`, false},
		{"PUT", `"a"`, `"a"`, false}, // never a 304 for a write
	} {
		r := httptest.NewRequest(tc.method, "/", nil)
		if tc.header != "" {
			r.Header.Set("If-None-Match", tc.header)
		}
		if got := httpapi.NotModified(r, tc.etag); got != tc.want {
			t.Errorf(
				"%s If-None-Match %s vs %s: %v, want %v",
				tc.method,
				tc.header,
				tc.etag,
				got,
				tc.want,
			)
		}
	}
}

func TestParsePage(t *testing.T) {
	cursor := httpapi.EncodeCursor([]byte("2026-10-08|42"))
	for _, tc := range []struct {
		query  string
		want   httpapi.Page
		reason string
	}{
		{"", httpapi.Page{Limit: 25}, ""},
		{"offset=40&limit=10", httpapi.Page{Offset: 40, Limit: 10}, ""},
		{"limit=100", httpapi.Page{Limit: 100}, ""},
		{"cursor=" + cursor, httpapi.Page{Limit: 25, Cursor: cursor}, ""},
		{"limit=101", httpapi.Page{}, "limit-too-large"},
		{"limit=0", httpapi.Page{Limit: 25}, ""},
		{"limit=-5", httpapi.Page{Limit: 25}, ""},
		{"limit=abc", httpapi.Page{}, "invalid-limit"},
		{"offset=-1", httpapi.Page{}, "invalid-offset"},
		{"offset=1&cursor=" + cursor, httpapi.Page{}, "offset-with-cursor"},
		{"cursor=***", httpapi.Page{}, "invalid-cursor"},
	} {
		q, _ := url.ParseQuery(tc.query)
		got, err := httpapi.ParsePage(q, httpapi.PageConfig{})
		if reason := apierr.Reason(err); reason != tc.reason {
			t.Errorf("%q: reason %q, want %q", tc.query, reason, tc.reason)
			continue
		}
		if err != nil && apierr.Status(err) != http.StatusBadRequest {
			t.Errorf("%q: status %d", tc.query, apierr.Status(err))
		}
		if err == nil && got != tc.want {
			t.Errorf("%q: %+v, want %+v", tc.query, got, tc.want)
		}
	}
}

func TestRender(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		reason  string
		message string
	}{
		{"apierr", apierr.NotFound("video-not-found", "no such video"), 404, "video-not-found", "no such video"},
		{"wrapped apierr", fmt.Errorf("load: %w", apierr.Forbidden("not-owner", "not yours")), 403, "not-owner", "not yours"},
		{"wrap without a message", apierr.Wrap("bad-input", 400, errors.New("strconv: parsing")), 400, "bad-input", "Bad Request"},
		{"server fault hides its message", apierr.Internal("db-down", errors.New("dial 10.0.0.5")), 500, "db-down", "an unexpected error occurred"},
		{"unclassified", errors.New("dial 10.0.0.5"), 500, "internal", "an unexpected error occurred"},
		{"body too large", fmt.Errorf("bind: %w", &http.MaxBytesError{Limit: 1}), 413, "body-too-large", "request body too large"},
	} {
		status, body := httpapi.Render(tc.err, "r")
		if status != tc.status || body.Reason != tc.reason ||
			body.Message != tc.message ||
			body.RequestID != "r" ||
			body.Status != "error" {
			t.Errorf("%s: %d %+v", tc.name, status, body)
		}
		if strings.Contains(body.Message, "10.0.0.5") {
			t.Errorf("%s: leaked the cause", tc.name)
		}
	}
}

func TestWriteErrorIsNeverCached(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", `"v1"`)
	httpapi.WriteError(w, apierr.NotFound("gone", "gone"), "r")
	if w.Header().Get("Cache-Control") != "no-store" ||
		w.Header().Get("ETag") != "" {
		t.Errorf("headers %v", w.Header())
	}
}

func TestRequestID(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(httpapi.RequestIDHeader, "trace-1:abc.DEF_2")
	if got := httpapi.RequestID(r); got != "trace-1:abc.DEF_2" {
		t.Errorf("a sane caller id was replaced: %q", got)
	}
	for _, bad := range []string{"", "has space", "line\nbreak", strings.Repeat("a", 129), `quote"`} {
		r.Header.Set(httpapi.RequestIDHeader, bad)
		if got := httpapi.RequestID(r); got == bad || len(got) != 36 {
			t.Errorf("caller id %q gave %q, want a new UUID", bad, got)
		}
	}
}

func TestMessageAndDataAreOptional(t *testing.T) {
	for _, tc := range []struct {
		envelope httpapi.Envelope
		want     string
	}{
		{httpapi.Success(map[string]int{"a": 1}), `{"status":"success","data":{"a":1}}`},
		{httpapi.Message("password changed"), `{"status":"success","message":"password changed"}`},
		{httpapi.Success(1).WithMessage("done"), `{"status":"success","message":"done","data":1}`},
		{httpapi.List("videos", []int(nil), 0, nil).WithMessage("none yet"), `{"status":"success","message":"none yet","data":{"videos":[],"total":0}}`},
	} {
		w := httptest.NewRecorder()
		if err := httpapi.WriteJSON(w, 200, tc.envelope); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(w.Body.String()); got != tc.want {
			t.Errorf("body %s, want %s", got, tc.want)
		}
	}
}

func TestRequestLineIsAiozStreamFormat(t *testing.T) {
	for _, tc := range []struct {
		method  string
		status  int
		latency time.Duration
		plain   bool
		want    string
	}{
		{"GET", 200, 12 * time.Millisecond, true, "GET status:200 latency:012ms url:/v1/videos/42"},
		{"POST", 201, 3 * time.Second, true, "PST status:201 latency:003s url:/v1/videos/42"},
		{"PATCH", 404, 250 * time.Microsecond, true, "PTC status:404 latency:250µs url:/v1/videos/42"},
		{"DELETE", 204, 900 * time.Nanosecond, true, "DEL status:204 latency:900ns url:/v1/videos/42"},
		{"OPTIONS", 204, time.Millisecond, true, "OPTIONS status:204 latency:001ms url:/v1/videos/42"},
		{"GET", 200, 12 * time.Millisecond, false, "\033[94mGET\033[0m status:\033[32m200\033[0m latency:012ms url:/v1/videos/42"},
		{"PUT", 500, 12 * time.Millisecond, false, "\033[93mPUT\033[0m status:\033[31m500\033[0m latency:012ms url:/v1/videos/42"},
		{"POST", 429, 12 * time.Millisecond, false, "\033[92mPST\033[0m status:\033[33m429\033[0m latency:012ms url:/v1/videos/42"},
	} {
		if got := httpapi.RequestLine(tc.method, tc.status, tc.latency, "/v1/videos/42", tc.plain); got != tc.want {
			t.Errorf("%s %d: %q, want %q", tc.method, tc.status, got, tc.want)
		}
	}
}

func TestLogAccessIsOneApiGroupRecord(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	httpapi.LogAccess(t.Context(), log, httpapi.Access{
		Method: "GET", Path: "/v1/videos/42", Route: "/v1/videos/:id", Status: 500,
		Latency: 12 * time.Millisecond, RequestID: "r-1", Plain: true,
		Err: errors.New("db down"),
	})
	var rec map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &rec); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	for k, want := range map[string]any{
		"level": "INFO", "msg": "GET status:500 latency:012ms url:/v1/videos/42",
		"group": "api", "request_id": "r-1", "route": "/v1/videos/:id",
		"reason": "internal", "err": "db down",
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestListIsAiozStreamShape(t *testing.T) {
	type video struct {
		ID string `json:"id"`
	}
	page := httpapi.Page{Offset: 25, Limit: 25}
	for _, tc := range []struct {
		envelope httpapi.Envelope
		want     string
	}{
		{httpapi.List("videos", []video{{"a"}}, 53, page),
			`{"status":"success","data":{"videos":[{"id":"a"}],"total":53},"query":{"offset":25,"limit":25}}`},
		{httpapi.List("videos", []video(nil), 0, page),
			`{"status":"success","data":{"videos":[],"total":0},"query":{"offset":25,"limit":25}}`},
		{httpapi.List("api_keys", []video{}, 0, nil),
			`{"status":"success","data":{"api_keys":[],"total":0}}`},
		{httpapi.CursorList("events", []video{{"a"}}, "eyJpZCI6NDJ9", httpapi.Page{Limit: 25}),
			`{"status":"success","data":{"events":[{"id":"a"}],"next_cursor":"eyJpZCI6NDJ9"},"query":{"offset":0,"limit":25}}`},
		{httpapi.CursorList("events", []video(nil), "", nil),
			`{"status":"success","data":{"events":[]}}`},
	} {
		w := httptest.NewRecorder()
		if err := httpapi.WriteJSON(w, 200, tc.envelope); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(w.Body.String()); got != tc.want {
			t.Errorf("body\n got %s\nwant %s", got, tc.want)
		}
	}
}
